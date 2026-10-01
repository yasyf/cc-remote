package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-remote/internal/state"
)

type Status struct {
	Provider   string            `json:"provider"`
	Profile    string            `json:"profile"`
	Workspaces []Record          `json:"workspaces"`
	Checks     map[string]string `json:"checks,omitempty"`
}

func (s *Session) Status() (*Status, error) {
	records, err := s.records()
	if err != nil {
		return nil, err
	}
	status := &Status{Provider: s.Kind, Profile: s.Profile, Workspaces: records}
	return status, nil
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
