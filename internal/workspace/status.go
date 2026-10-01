package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/yasyf/cc-remote/internal/budget"
	"github.com/yasyf/cc-remote/internal/state"
)

type Status struct {
	Provider   string            `json:"provider"`
	Profile    string            `json:"profile"`
	Workspaces []Record          `json:"workspaces"`
	Pool       PoolStatus        `json:"pool"`
	Budget     BudgetStatus      `json:"budget"`
	Resources  []ResourceStatus  `json:"resources"`
	Checks     map[string]string `json:"checks,omitempty"`
}

type PoolStatus struct {
	Fingerprint string                   `json:"fingerprint"`
	Target      int                      `json:"target"`
	Pooled      int                      `json:"pooled"`
	Spares      map[string]*budget.Spare `json:"spares"`
}

type BudgetStatus struct {
	Ledger       string  `json:"ledger"`
	CapUSD       float64 `json:"capUSD"`
	ReserveUSD   float64 `json:"reserveUSD"`
	SpentUSD     float64 `json:"spentUSD"`
	CommittedUSD float64 `json:"committedUSD"`
	RemainingUSD float64 `json:"remainingUSD"`
	EstimateUSD  float64 `json:"estimateUSD"`
}

type ResourceStatus struct {
	ID       string  `json:"id"`
	Provider string  `json:"provider"`
	Profile  string  `json:"profile"`
	Running  bool    `json:"running"`
	Retired  bool    `json:"retired"`
	SpentUSD float64 `json:"spentUSD"`
}

func (s *Session) Status() (*Status, error) {
	records, err := s.records()
	if err != nil {
		return nil, err
	}
	ledger, err := s.Pool.Ledger.Read()
	if err != nil {
		return nil, err
	}
	spares, err := s.Pool.Ledger.ReadSpares()
	if err != nil {
		return nil, err
	}
	spares.DropRetired(ledger)
	now := s.Now()
	status := &Status{
		Provider:   s.Kind,
		Profile:    s.Profile,
		Workspaces: records,
		Pool:       PoolStatus{Fingerprint: s.Pool.Fingerprint, Target: s.Pool.Spares, Pooled: spares.Pooled(s.Pool.Fingerprint), Spares: map[string]*budget.Spare{}},
		Budget: BudgetStatus{
			Ledger:       s.Pool.Ledger.Path,
			CapUSD:       s.Pool.Guard.CapUSD,
			ReserveUSD:   s.Pool.Guard.ReserveUSD,
			SpentUSD:     ledger.Spent(now),
			CommittedUSD: ledger.Committed(now),
			RemainingUSD: s.Pool.Guard.Remaining(ledger, now),
			EstimateUSD:  s.Pool.Estimate,
		},
	}
	for id, spare := range spares {
		if resource := ledger.Resources[id]; resource.Provider == s.Kind && resource.Profile == s.Profile {
			status.Pool.Spares[id] = spare
		}
	}
	for _, id := range slices.Sorted(mapKeys(ledger.Resources)) {
		resource := ledger.Resources[id]
		status.Resources = append(status.Resources, ResourceStatus{
			ID:       id,
			Provider: resource.Provider,
			Profile:  resource.Profile,
			Running:  ledger.Running(id),
			Retired:  resource.Destroyed != nil,
			SpentUSD: resource.Spent(now),
		})
	}
	return status, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

func (s *Session) records() ([]Record, error) {
	dir := filepath.Dir(s.State.Workspace("x"))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		var record Record
		if _, err := state.Load(s.State.Workspace(name), &record); err != nil {
			return nil, err
		}
		if record.Provider == s.Kind && record.Profile == s.Profile {
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *Session) Verify(ctx context.Context) map[string]string {
	checks := map[string]string{}
	report := func(name string, err error) {
		if err != nil {
			checks[name] = err.Error()
			return
		}
		checks[name] = "ok"
	}
	report("config", nil)
	report("state", s.stateWritable())
	report("provider", s.Provider.Check(ctx))
	if TokenAllowed(s.Config.Repository) {
		_, err := s.Token(ctx)
		report("git-token", err)
	}
	if s.Enroller != nil {
		report("tailnet", s.verifyTailnet(ctx))
	}
	if s.Config.Bootstrap != "" {
		_, err := os.Stat(s.Config.ScriptPath(s.Config.Bootstrap))
		report("bootstrap", err)
	}
	report("ssh", lookPath("ssh"))
	return checks
}

func (s *Session) stateWritable() error {
	probe := filepath.Join(string(s.State), ".probe")
	if err := os.MkdirAll(string(s.State), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

func lookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}

func StartDetached(argv []string, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, errors.Join(err, log.Close())
	}
	pid := cmd.Process.Pid
	return pid, errors.Join(cmd.Process.Release(), log.Close())
}
