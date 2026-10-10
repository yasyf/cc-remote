package workspacetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const Token = "ghp_contracttokenthatmustneverlandondisk0000"

type Harness struct {
	Provider providers.Provider
	Kind     string
	Platform workspace.Platform
	Root     string
	Machine  config.Machine
}

func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	tests := []struct {
		name string
		run  func(t *testing.T, h Harness, s *workspace.Session)
	}{
		{"CreateSuspendResumeDestroy", createSuspendResumeDestroy},
		{"CreateRefusesADuplicateName", createRefusesADuplicate},
		{"TokenNeverReachesAScriptOrDisk", tokenNeverReachesAScriptOrDisk},
		{"VerifyReportsEveryCheck", verifyReportsEveryCheck},
		{"CreateWorksFromEmptyState", createWorksFromEmptyState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.run(t, h, Open(t, h))
		})
	}
}

func GitRepository(t *testing.T) string {
	t.Helper()
	origin := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", origin, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "README"), []byte("app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	git("branch", "-q", "feature")
	return "file://" + origin
}

const Inventory = "version: 1\nconfigure:\n  env: [WEB_PORT]\n"

func Config(t *testing.T, h Harness) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(Inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
repository: %s
ref: main
provider: %s
profile: lean
state_dir: %s
providers:
  %s: {}
workspace_dirs:
  %s: %s
profiles:
  lean:
    checkout: shallow
    prepare: ["echo prepared >> \"$HOME/steps\""]
    machine:
      %s: { image: %q, size: %q, region: %q }
inventory: ./inventory.yaml
forwards:
  - { label: web, env: WEB_PORT }
`, GitRepository(t), h.Kind, t.TempDir(), h.Kind, h.Kind, h.Root, h.Kind, h.Machine.Image, h.Machine.Size, h.Machine.Region)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(dir, "config.yaml")
	return cfg
}

func Open(t *testing.T, h Harness) *workspace.Session {
	t.Helper()
	s, err := workspace.Open(Config(t, h), h.Provider, h.Kind, "lean", h.Platform)
	if err != nil {
		t.Fatal(err)
	}
	s.Token = func(context.Context) (string, error) { return Token, nil }
	s.Stderr = &testWriter{t: t}
	s.Log = slog.New(slog.DiscardHandler)
	return s
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	if text := strings.TrimSpace(string(p)); text != "" {
		w.t.Log(text)
	}
	return len(p), nil
}

func record(t *testing.T, s *workspace.Session, name string) (workspace.Record, bool) {
	t.Helper()
	var record workspace.Record
	found, err := state.Load(s.State.Workspace(name), &record)
	if err != nil {
		t.Fatal(err)
	}
	return record, found
}

func createSuspendResumeDestroy(t *testing.T, h Harness, s *workspace.Session) {
	ctx := t.Context()
	result, err := s.Create(ctx, "ws-1", workspace.Source{Ref: "main"})
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if result.SchemaVersion != workspace.SchemaVersion || result.Name != "ws-1" || result.Provider != h.Kind || result.Machine == "" || result.ProjectRoot != s.ProjectRoot() || len(result.Forwards) != 1 || result.Forwards[0].Label != "web" || result.Forwards[0].Port == 0 {
		t.Errorf("result = %+v", result)
	}
	switch {
	case result.SSH != nil && result.Compute == nil:
		if fragment, err := os.ReadFile(result.SSH.Config); err != nil || result.SSH.Config != s.State.SSH("ws-1") || !strings.HasPrefix(string(fragment), "Host ws-1\n") || !strings.Contains(string(fragment), "\n  HostName "+result.SSH.Host+"\n") {
			t.Errorf("ssh fragment %s = %q, %v", result.SSH.Config, fragment, err)
		}
	case result.Compute != nil && result.SSH == nil:
		if _, err := os.Stat(s.State.SSH("ws-1")); !errors.Is(err, os.ErrNotExist) || result.Compute.InstanceID != result.Machine {
			t.Errorf("compute result %+v wrote an ssh fragment or names another instance: %v", result.Compute, err)
		}
	default:
		t.Errorf("result carries ssh %+v and compute %+v, want exactly one access", result.SSH, result.Compute)
	}
	machine, err := h.Provider.Get(ctx, result.Machine)
	if err != nil || machine.Labels[workspace.LabelWorkspace] != "ws-1" || machine.Labels[workspace.LabelProfile] != "lean" {
		t.Errorf("machine = %+v, %v", machine, err)
	}
	if rec, found := record(t, s, "ws-1"); !found || rec.Machine != result.Machine || rec.Source.Ref != "main" || len(rec.Forwards) != 1 {
		t.Errorf("record = %+v, found %v", rec, found)
	}
	if err := s.Suspend(ctx, "ws-1"); err != nil {
		t.Fatalf("Suspend = %v", err)
	}
	resumed, err := s.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if resumed.Forwards[0].Port != result.Forwards[0].Port {
		t.Errorf("resume allocated %+v over %+v", resumed.Forwards, result.Forwards)
	}
	if err := s.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.Provider.Get(ctx, result.Machine); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the destroyed machine is still there: %v", err)
	}
	if _, found := record(t, s, "ws-1"); found {
		t.Error("the destroyed workspace is still recorded")
	}
	if _, err := os.Stat(s.State.SSH("ws-1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the destroyed workspace still has an ssh fragment")
	}
	if _, err := s.Resume(ctx, "ws-1"); err == nil {
		t.Error("a destroyed workspace resumed")
	}
	if _, err := s.Create(ctx, "ws-1", workspace.Source{Ref: "main"}); err != nil {
		t.Errorf("a destroyed name could not be created again: %v", err)
	}
}

func createWorksFromEmptyState(t *testing.T, h Harness, _ *workspace.Session) {
	s, err := workspace.Open(Config(t, h), h.Provider, h.Kind, "lean", h.Platform)
	if err != nil {
		t.Fatal(err)
	}
	s.Log = slog.New(slog.DiscardHandler)
	s.Token = func(context.Context) (string, error) { return Token, nil }
	result, err := s.Create(t.Context(), "ws-5", workspace.Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := h.Provider.Get(t.Context(), result.Machine)
	if err != nil || machine.Labels[workspace.LabelWorkspace] != "ws-5" {
		t.Fatalf("fresh machine = %+v, %v", machine, err)
	}
	if rec, found := record(t, s, "ws-5"); !found || rec.Machine != result.Machine || rec.Unverified {
		t.Fatalf("fresh record = %+v, found %v", rec, found)
	}
}

func createRefusesADuplicate(t *testing.T, _ Harness, s *workspace.Session) {
	ctx := t.Context()
	if _, err := s.Create(ctx, "ws-2", workspace.Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "ws-2", workspace.Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("a second create of the same name: %v", err)
	}
	if _, err := s.Create(ctx, "Bad Name", workspace.Source{Ref: "main"}); err == nil {
		t.Error("an invalid name was created")
	}
	if rec, found := record(t, s, "ws-2"); !found || rec.Machine != "ws-2" {
		t.Errorf("the refused retry changed the record: %+v, %v", rec, found)
	}
}

func tokenNeverReachesAScriptOrDisk(t *testing.T, h Harness, s *workspace.Session) {
	ctx := t.Context()
	seen := &capture{Provider: h.Provider}
	s.Provider = seen
	if _, err := s.Create(ctx, "ws-4", workspace.Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range seen.commands {
		if strings.Contains(strings.Join(cmd, " "), Token) {
			t.Error("the git token was part of a command")
		}
	}
	if !seen.stdin {
		t.Error("no command read stdin, so the token reached the machine some other way")
	}
	for _, path := range []string{s.State.Workspace("ws-4"), s.State.SSH("ws-4")} {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && path == s.State.SSH("ws-4") {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), Token) {
			t.Errorf("%s carries the git token", path)
		}
	}
}

func verifyReportsEveryCheck(t *testing.T, _ Harness, s *workspace.Session) {
	checks := s.Verify(t.Context())
	for _, name := range []string{"config", "state", "provider", "ssh"} {
		if checks[name] != "ok" {
			t.Errorf("check %s = %q", name, checks[name])
		}
	}
	if _, ok := checks["tailnet"]; ok {
		t.Error("a config with no tailnet verified one")
	}
}

type capture struct {
	providers.Provider
	mu       sync.Mutex
	commands [][]string
	stdin    bool
}

func (c *capture) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	c.mu.Lock()
	c.commands = append(c.commands, append([]string(nil), cmd...))
	if stdin != nil {
		c.stdin = true
	}
	c.mu.Unlock()
	return c.Provider.Exec(ctx, id, cmd, stdin)
}
