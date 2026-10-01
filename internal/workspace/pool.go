package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/yasyf/cc-remote/internal/budget"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/remote"
)

const sparePrefix = remote.Prefix + "-spare-"

type Pool struct {
	Provider    string
	Profile     string
	Fingerprint string
	Spares      int
	Rate        budget.Rate
	Estimate    float64
	Guard       budget.Guard
	Ledger      budget.Store
}

func NewPool(cfg *config.Config, provider, profile string, rate budget.Rate) (Pool, error) {
	prepared, err := cfg.ProfileNamed(profile)
	if err != nil {
		return Pool{}, err
	}
	section, err := cfg.RawProviderSection(provider)
	if err != nil {
		return Pool{}, err
	}
	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode(struct {
		Repository, Provider, Profile, Root string
		Section                             string
		Spec                                config.Profile
	}{cfg.Repository, provider, profile, cfg.ProjectRoot(provider), string(section), prepared}); err != nil {
		return Pool{}, err
	}
	if cfg.Bootstrap != "" {
		if err := hashScript(hash, cfg.Bootstrap, cfg.ScriptPath(cfg.Bootstrap)); err != nil {
			return Pool{}, err
		}
	}
	return Pool{
		Provider:    provider,
		Profile:     profile,
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
		Spares:      cfg.SpareCount(provider, profile),
		Rate:        rate,
		Estimate:    rate.Estimate(cfg.Budget.TrialHours),
		Guard:       budget.Guard{CapUSD: cfg.Budget.CapUSD, ReserveUSD: cfg.Budget.ReserveUSD},
		Ledger:      budget.Store{Path: cfg.State().Ledger(cfg.Budget.Ledger)},
	}, nil
}

func hashScript(hash io.Writer, name, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(hash, "%s %d\n", name, len(raw)); err != nil {
		return err
	}
	_, err = hash.Write(raw)
	return err
}

func (p Pool) SpareName() string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s%s-%s-%s", sparePrefix, p.Profile, p.Fingerprint[:12], hex.EncodeToString(suffix))
}

type Assignment string

const (
	FreshCreate Assignment = "fresh"
	NewClaim    Assignment = "claim"
	Reclaim     Assignment = "reclaim"
)

func (p Pool) Assign(request string, now time.Time) (string, Assignment, error) {
	var name string
	var assignment Assignment
	err := p.Ledger.UpdateSpares(func(ledger *budget.Ledger, spares budget.Spares) error {
		if held, ok := spares.HeldBy(request); ok {
			if spares[held].ActivatedAt == nil {
				return fmt.Errorf("%s was claimed for %s by a create that never finished; it is left as it is, so destroy %s before creating again", held, request, request)
			}
			name, assignment = held, Reclaim
		} else if err := ledger.Vacant(request); err != nil {
			return err
		} else if claimed, ok := spares.Claim(p.Fingerprint, request, now); ok {
			name, assignment = claimed, NewClaim
		} else {
			name, assignment = request, FreshCreate
		}
		if assignment == Reclaim && ledger.Running(name) {
			return nil
		}
		if err := p.Guard.Admit(ledger, p.Estimate, now); err != nil {
			return err
		}
		return ledger.Start(name, p.Provider, p.Profile, p.Rate, p.Estimate, now)
	})
	return name, assignment, err
}

func (p Pool) Claimed(request string) (string, error) {
	machine, _, err := p.Ledger.ClaimedFor(request)
	return machine, err
}

func (p Pool) Activate(name string, now time.Time) error {
	return p.Ledger.UpdateSpares(func(_ *budget.Ledger, spares budget.Spares) error {
		return spares.Activate(name, now)
	})
}

func (p Pool) Reserve(name string, preparer int, now time.Time) (bool, error) {
	var reserved bool
	err := p.Ledger.UpdateSpares(func(ledger *budget.Ledger, spares budget.Spares) error {
		if spares.Pooled(p.Fingerprint) >= p.Spares {
			return nil
		}
		if err := p.Guard.Admit(ledger, p.Estimate, now); err != nil {
			return err
		}
		reserved = true
		return spares.Prepare(ledger, name, p.Provider, p.Profile, p.Fingerprint, p.Rate, p.Estimate, preparer, now)
	})
	return reserved, err
}

func (p Pool) MarkReady(name string, now time.Time) error {
	return p.Ledger.UpdateSpares(func(ledger *budget.Ledger, spares budget.Spares) error {
		return spares.MarkReady(ledger, name, now)
	})
}

func (p Pool) Drain(all bool) ([]string, error) {
	var drained []string
	err := p.Ledger.UpdateSpares(func(ledger *budget.Ledger, spares budget.Spares) error {
		drained = spares.Drain(func(id string, spare *budget.Spare) bool {
			resource := ledger.Resources[id]
			if resource.Provider != p.Provider || resource.Profile != p.Profile {
				return false
			}
			switch spare.State {
			case budget.Draining:
				return true
			case budget.Preparing:
				return !alive(spare.Preparer)
			}
			return all || spare.Fingerprint != p.Fingerprint
		})
		return nil
	})
	return drained, err
}

func (p Pool) Start(name string, now time.Time) error {
	return p.Ledger.Update(func(ledger *budget.Ledger) error {
		if err := p.Guard.Admit(ledger, p.Estimate, now); err != nil {
			return err
		}
		return ledger.Start(name, p.Provider, p.Profile, p.Rate, p.Estimate, now)
	})
}

func (p Pool) Stop(name string, now time.Time) error {
	return p.Ledger.Update(func(ledger *budget.Ledger) error {
		return ledger.Stop(name, now)
	})
}

func (p Pool) Retire(name string, now time.Time) error {
	return p.Ledger.Update(func(ledger *budget.Ledger) error {
		return ledger.Retire(name, now)
	})
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
