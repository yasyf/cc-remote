package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/remote"
	sparestate "github.com/yasyf/cc-remote/internal/spare"
	"github.com/yasyf/cc-remote/internal/state"
)

const (
	sparePrefix      = remote.Prefix + "-spare-"
	spareFingerprint = 12
)

type Pool struct {
	Provider    string
	Profile     string
	Fingerprint string
	Spares      int
	Store       sparestate.Store
}

func NewPool(cfg *config.Config, provider, profile string, stamp string) (Pool, error) {
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
		Stamp                               string
	}{cfg.Repository, provider, profile, cfg.ProjectRoot(provider), string(section), prepared, stamp}); err != nil {
		return Pool{}, err
	}
	return Pool{
		Provider:    provider,
		Profile:     profile,
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
		Spares:      cfg.SpareCount(provider, profile),
		Store:       sparestate.Store{Path: cfg.State().Spares()},
	}, nil
}

func (p Pool) SpareName() string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	slug := p.Profile
	if limit := state.NameLimit - len(sparePrefix) - 2 - spareFingerprint - 2*len(suffix); len(slug) > limit {
		slug = strings.TrimRight(slug[:limit], "-")
	}
	return fmt.Sprintf("%s%s-%s-%s", sparePrefix, slug, p.Fingerprint[:spareFingerprint], hex.EncodeToString(suffix))
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
	err := p.Store.Update(func(spares sparestate.Spares) error {
		if held, ok := spares.HeldBy(request); ok {
			if spares[held].ActivatedAt == nil {
				return fmt.Errorf("%s was claimed for %s by a create that never finished; it is left as it is, so destroy %s before creating again", held, request, request)
			}
			name, assignment = held, Reclaim
		} else if _, exists := spares[request]; exists {
			return fmt.Errorf("%s already exists as a spare; it is left as it is, so use a different workspace name", request)
		} else if claimed, ok := spares.Claim(p.Fingerprint, request, now); ok {
			name, assignment = claimed, NewClaim
		} else {
			name, assignment = request, FreshCreate
		}
		return nil
	})
	return name, assignment, err
}

func (p Pool) Claimed(request string) (string, error) {
	machine, _, err := p.Store.ClaimedFor(request)
	return machine, err
}

func (p Pool) Activate(name string, now time.Time) error {
	return p.Store.Update(func(spares sparestate.Spares) error { return spares.Activate(name, now) })
}

func (p Pool) BeginPrepare(name string, preparer int) (bool, error) {
	var reserved bool
	err := p.Store.Update(func(spares sparestate.Spares) error {
		if spares.Pooled(p.Fingerprint) >= p.Spares {
			return nil
		}
		reserved = true
		return spares.Prepare(name, p.Provider, p.Profile, p.Fingerprint, preparer)
	})
	return reserved, err
}

func (p Pool) MarkReady(name string, now time.Time) error {
	return p.Store.Update(func(spares sparestate.Spares) error { return spares.MarkReady(name, now) })
}

func (p Pool) Drain(all bool) ([]string, error) {
	var drained []string
	err := p.Store.Update(func(spares sparestate.Spares) error {
		drained = spares.Drain(func(_ string, item *sparestate.Spare) bool {
			if item.Provider != p.Provider || item.Profile != p.Profile {
				return false
			}
			switch item.State {
			case sparestate.Draining:
				return true
			case sparestate.Preparing:
				return !alive(item.Preparer)
			}
			return all || item.Fingerprint != p.Fingerprint
		})
		return nil
	})
	return drained, err
}

func (p Pool) Remove(name string) error {
	return p.Store.Update(func(spares sparestate.Spares) error { delete(spares, name); return nil })
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
