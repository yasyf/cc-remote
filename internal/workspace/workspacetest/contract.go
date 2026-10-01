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
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/budget"
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
	Spares   int
}

func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	tests := []struct {
		name string
		run  func(t *testing.T, h Harness, s *workspace.Session)
	}{
		{"CreateSuspendResumeDestroy", createSuspendResumeDestroy},
		{"CreateRefusesADuplicateName", createRefusesADuplicate},
		{"PrepareFillsThePoolAndCreateClaims", prepareFillsThePoolAndCreateClaims},
		{"DrainDestroysUnclaimedSpares", drainDestroysUnclaimedSpares},
		{"TokenNeverReachesAScriptOrDisk", tokenNeverReachesAScriptOrDisk},
		{"VerifyReportsEveryCheck", verifyReportsEveryCheck},
		{"CreateRefusesWithoutALedger", createRefusesWithoutALedger},
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
    warm: ["echo warmed >> \"$HOME/steps\""]
    warm_inputs: [README]
    machine:
      %s: { image: %q, size: %q, region: %q }
spares:
  %s: { lean: %d }
inventory: ./inventory.yaml
forwards:
  - { label: web, env: WEB_PORT }
budget:
  cap_usd: 1000
  reserve_usd: 10
  trial_hours: 1
`, GitRepository(t), h.Kind, t.TempDir(), h.Kind, h.Kind, h.Root, h.Kind, h.Machine.Image, h.Machine.Size, h.Machine.Region, h.Kind, h.Spares)))
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
	if err := s.Pool.Ledger.Init(time.Now()); err != nil {
		t.Fatal(err)
	}
	return s
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	if text := strings.TrimSpace(string(p)); text != "" {
		w.t.Log(text)
	}
	return len(p), nil
}

func ledger(t *testing.T, s *workspace.Session) *budget.Ledger {
	t.Helper()
	ledger, err := s.Pool.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func spares(t *testing.T, s *workspace.Session) budget.Spares {
	t.Helper()
	spares, err := s.Pool.Ledger.ReadSpares()
	if err != nil {
		t.Fatal(err)
	}
	spares.DropRetired(ledger(t, s))
	return spares
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
	if result.SchemaVersion != workspace.SchemaVersion || result.Name != "ws-1" || result.Provider != h.Kind || result.Machine != "ws-1" || result.ProjectRoot != s.ProjectRoot() || result.SSH.Host == "" || len(result.Forwards) != 1 || result.Forwards[0].Label != "web" || result.Forwards[0].Port == 0 {
		t.Errorf("result = %+v", result)
	}
	if fragment, err := os.ReadFile(result.SSH.Config); err != nil || result.SSH.Config != s.State.SSH("ws-1") || !strings.HasPrefix(string(fragment), "Host ws-1\n") || !strings.Contains(string(fragment), "\n  HostName "+result.SSH.Host+"\n") {
		t.Errorf("ssh fragment %s = %q, %v", result.SSH.Config, fragment, err)
	}
	machine, err := h.Provider.Get(ctx, "ws-1")
	if err != nil || machine.Labels[workspace.LabelWorkspace] != "ws-1" || machine.Labels[workspace.LabelProfile] != "lean" {
		t.Errorf("machine = %+v, %v", machine, err)
	}
	if rec, found := record(t, s, "ws-1"); !found || rec.Machine != "ws-1" || rec.Source.Ref != "main" || rec.Claimed || len(rec.Forwards) != 1 {
		t.Errorf("record = %+v, found %v", rec, found)
	}
	if !ledger(t, s).Running("ws-1") {
		t.Error("the ledger does not run the created workspace")
	}
	if err := s.Suspend(ctx, "ws-1"); err != nil {
		t.Fatalf("Suspend = %v", err)
	}
	if ledger(t, s).Running("ws-1") {
		t.Error("the ledger still runs the suspended workspace")
	}
	resumed, err := s.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if resumed.Forwards[0].Port != result.Forwards[0].Port || !ledger(t, s).Running("ws-1") {
		t.Errorf("resume allocated %+v over %+v, running %v", resumed.Forwards, result.Forwards, ledger(t, s).Running("ws-1"))
	}
	if err := s.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.Provider.Get(ctx, "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the destroyed machine is still there: %v", err)
	}
	if _, found := record(t, s, "ws-1"); found {
		t.Error("the destroyed workspace is still recorded")
	}
	if _, err := os.Stat(result.SSH.Config); !errors.Is(err, os.ErrNotExist) {
		t.Error("the destroyed workspace still has an ssh fragment")
	}
	if resource := ledger(t, s).Resources["ws-1"]; resource == nil || resource.Destroyed == nil {
		t.Errorf("the ledger did not retire the workspace: %+v", resource)
	}
	if _, err := s.Resume(ctx, "ws-1"); err == nil {
		t.Error("a destroyed workspace resumed")
	}
	if _, err := s.Create(ctx, "ws-1", workspace.Source{Ref: "main"}); err != nil {
		t.Errorf("a destroyed name could not be created again: %v", err)
	}
}

func createRefusesWithoutALedger(t *testing.T, h Harness, _ *workspace.Session) {
	s, err := workspace.Open(Config(t, h), h.Provider, h.Kind, "lean", h.Platform)
	if err != nil {
		t.Fatal(err)
	}
	s.Log = slog.New(slog.DiscardHandler)
	if _, err := s.Create(t.Context(), "ws-5", workspace.Source{Ref: "main"}); !errors.Is(err, budget.ErrNoLedger) {
		t.Errorf("a create with no ledger: %v", err)
	}
	if err := s.Prepare(t.Context()); !errors.Is(err, budget.ErrNoLedger) {
		t.Errorf("a prepare with no ledger: %v", err)
	}
	if _, err := h.Provider.Get(t.Context(), "ws-5"); !errors.Is(err, providers.ErrNotFound) {
		t.Error("a machine was created without a ledger")
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

func prepareFillsThePoolAndCreateClaims(t *testing.T, h Harness, s *workspace.Session) {
	if h.Spares == 0 {
		t.Skip("the harness keeps no spares")
	}
	ctx := t.Context()
	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	pooled := spares(t, s)
	if len(pooled) != h.Spares {
		t.Fatalf("the pool holds %d spares, want %d", len(pooled), h.Spares)
	}
	for id, spare := range pooled {
		if spare.State != budget.Ready || spare.Fingerprint != s.Pool.Fingerprint || !strings.HasPrefix(id, "cc-remote-spare-lean-") {
			t.Errorf("spare %s = %+v", id, spare)
		}
		if machine, err := h.Provider.Get(ctx, id); err != nil || machine.State != providers.StateSuspended || machine.Labels[workspace.LabelSpare] != s.Pool.Fingerprint {
			t.Errorf("spare machine %s = %+v, %v", id, machine, err)
		}
		if ledger(t, s).Running(id) {
			t.Errorf("a ready spare %s still accrues compute", id)
		}
	}
	refills := 0
	s.Refill = func() error { refills++; return nil }
	result, err := s.Create(ctx, "ws-3", workspace.Source{Ref: "feature"})
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if _, isSpare := pooled[result.Machine]; !isSpare || result.Name != "ws-3" || result.Source.Ref != "feature" {
		t.Errorf("create did not claim a spare: %+v", result)
	}
	if refills != 1 {
		t.Errorf("a claim started %d refills", refills)
	}
	if spare := spares(t, s)[result.Machine]; spare == nil || spare.State != budget.Claimed || spare.Request != "ws-3" || spare.ActivatedAt == nil {
		t.Errorf("the claimed spare is %+v", spare)
	}
	if rec, found := record(t, s, "ws-3"); !found || !rec.Claimed || rec.Machine != result.Machine {
		t.Errorf("record = %+v, found %v", rec, found)
	}
	if err := s.Destroy(ctx, "ws-3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := spares(t, s)[result.Machine]; ok {
		t.Error("the destroyed workspace is still in the pool")
	}
}

func drainDestroysUnclaimedSpares(t *testing.T, h Harness, s *workspace.Session) {
	if h.Spares == 0 {
		t.Skip("the harness keeps no spares")
	}
	ctx := t.Context()
	if err := s.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(ctx, true); err != nil {
		t.Fatalf("Drain = %v", err)
	}
	if left := spares(t, s); len(left) != 0 {
		t.Errorf("the pool still holds %v", left)
	}
	machines, err := h.Provider.List(ctx, map[string]string{workspace.LabelSpare: s.Pool.Fingerprint})
	if err != nil || len(machines) != 0 {
		t.Errorf("drained spares still exist: %v, %v", machines, err)
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
	status, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.State.Workspace("ws-4"), status.Budget.Ledger} {
		raw, err := os.ReadFile(path)
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
	for _, name := range []string{"config", "state", "ledger", "provider", "ssh"} {
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
	commands [][]string
	stdin    bool
}

func (c *capture) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	c.commands = append(c.commands, append([]string(nil), cmd...))
	if stdin != nil {
		c.stdin = true
	}
	return c.Provider.Exec(ctx, id, cmd, stdin)
}
