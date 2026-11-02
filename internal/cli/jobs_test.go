package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
)

type jobMachines struct {
	providers.Provider
	mu     sync.Mutex
	calls  []string
	stdin  map[string]string
	exitOn map[string]int
}

func (m *jobMachines) Exec(_ context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, id+" "+strings.Join(cmd, " "))
	if stdin != nil {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return providers.Result{}, err
		}
		m.stdin[id] = string(raw)
	}
	return providers.Result{ExitCode: m.exitOn[id+" "+cmd[0]]}, nil
}

type jobsWorld struct {
	pool     *jobPool
	machines *jobMachines
	clock    time.Time
}

func newJobsWorld(t *testing.T, jobs config.Jobs) *jobsWorld {
	t.Helper()
	dir := t.TempDir()
	w := &jobsWorld{machines: &jobMachines{stdin: map[string]string{}, exitOn: map[string]int{}}, clock: time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)}
	cfg := &config.Config{Repository: "https://github.com/acme/app", StateDir: filepath.Join(dir, "state"), Jobs: jobs, Path: filepath.Join(dir, "config.yaml")}
	w.pool = &jobPool{
		cfg:      cfg,
		dir:      cfg.State(),
		provider: w.machines,
		token:    func(context.Context) (string, error) { return "ghs_short", nil },
		now:      func() time.Time { return w.clock },
		log:      slog.New(slog.DiscardHandler),
	}
	return w
}

func (w *jobsWorld) member(t *testing.T, name string) *jobMember {
	t.Helper()
	member, err := w.pool.load(name)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

func (w *jobsWorld) adopt(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := w.pool.adopt([]string{name}); err != nil {
			t.Fatal(err)
		}
		w.clock = w.clock.Add(time.Second)
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJobsClaimHoldsTheOldestReadyMachineAndGivesItAGitTokenOverStdin(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	w.adopt(t, "fleet-b", "fleet-a")

	name, err := w.pool.claim(t.Context(), "restack-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if name != "fleet-b" {
		t.Fatalf("claimed %s, want the oldest ready machine fleet-b", name)
	}
	member := w.member(t, "fleet-b")
	if member.State != jobHeld || member.Holder != "restack-1" || !member.TokenAt.Equal(w.clock) {
		t.Fatalf("claimed record = %+v, want held by restack-1 with its token time", member)
	}
	if got := w.machines.stdin["fleet-b"]; got != "protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_short\n\n" {
		t.Fatalf("credential stdin = %q", got)
	}
	if slices.ContainsFunc(w.machines.calls, func(call string) bool { return strings.Contains(call, "ghs_short") }) {
		t.Fatalf("the token reached a command line: %q", w.machines.calls)
	}
	if w.member(t, "fleet-a").State != jobReady {
		t.Fatal("the younger machine left ready")
	}
}

func TestJobsClaimMovesPastAMachineThatRefusesTheCredential(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	w.adopt(t, "fleet-a", "fleet-b")
	w.machines.exitOn["fleet-a git"] = 1

	name, err := w.pool.claim(t.Context(), "vet-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if name != "fleet-b" {
		t.Fatalf("claimed %s, want fleet-b", name)
	}
	if member := w.member(t, "fleet-a"); member.State != jobFailed || !strings.Contains(member.Reason, "credential") {
		t.Fatalf("refusing machine = %+v, want failed for its credential", member)
	}
}

func TestJobsClaimWithNothingReadyFailsNamingTheHolder(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	w.adopt(t, "fleet-a")
	if _, err := w.pool.claim(t.Context(), "first", 0); err != nil {
		t.Fatal(err)
	}

	_, err := w.pool.claim(t.Context(), "second", 0)
	if err == nil || !strings.Contains(err.Error(), "no idle job machine for second") {
		t.Fatalf("claim on an empty pool = %v", err)
	}
}

func TestJobsReleaseResetsTheMachineAndReturnsItToReady(t *testing.T) {
	tests := []struct {
		name  string
		exit  int
		state jobState
	}{
		{name: "reset passes", exit: 0, state: jobReady},
		{name: "reset fails", exit: 3, state: jobFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newJobsWorld(t, config.Jobs{Reset: []string{"sh", "-c", "git worktree prune"}})
			w.adopt(t, "fleet-a")
			if _, err := w.pool.claim(t.Context(), "lane", 0); err != nil {
				t.Fatal(err)
			}
			w.machines.exitOn["fleet-a sh"] = tt.exit

			err := w.pool.release(t.Context(), "fleet-a")
			if (err != nil) != (tt.exit != 0) {
				t.Fatalf("release = %v", err)
			}
			member := w.member(t, "fleet-a")
			if member.State != tt.state || (tt.state == jobReady && member.Holder != "") {
				t.Fatalf("released record = %+v, want %s", member, tt.state)
			}
			if !slices.Contains(w.machines.calls, "fleet-a sh -c git worktree prune") {
				t.Fatalf("calls = %q, want the reset", w.machines.calls)
			}
		})
	}
}

func TestJobsReleaseRefusesAMachineThatIsNotHeld(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	w.adopt(t, "fleet-a")

	if err := w.pool.release(t.Context(), "fleet-a"); err == nil || !strings.Contains(err.Error(), "is ready, not held") {
		t.Fatalf("release of a ready machine = %v", err)
	}
}

func TestJobsKeepProvisionsTheShortfallAndRefreshesStaleTokens(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{Ready: 3, Prefix: "job", Provision: []string{script(t, `case "$1" in job-bad*) exit 4;; esac; echo "made $1"`)}})
	names := []string{"job-bad00001", "job-good0002"}
	named := jobName
	t.Cleanup(func() { jobName = named })
	jobName = func(string) string {
		name := names[0]
		names = names[1:]
		return name
	}
	w.adopt(t, "fleet-a", "fleet-b")
	if _, err := w.pool.claim(t.Context(), "long-lane", 0); err != nil {
		t.Fatal(err)
	}
	w.clock = w.clock.Add(tokenRefresh)

	kept, err := w.pool.keep(t.Context())
	if err == nil || !strings.Contains(err.Error(), "job-bad00001") {
		t.Fatalf("keep error = %v, want the failed provision named", err)
	}
	if !slices.Equal(kept.Created, []string{"job-good0002"}) || !slices.Equal(kept.Failed, []string{"job-bad00001"}) || !slices.Equal(kept.Refreshed, []string{"fleet-a"}) {
		t.Fatalf("keep = %+v", kept)
	}
	if member := w.member(t, "job-good0002"); member.State != jobReady {
		t.Fatalf("provisioned record = %+v", member)
	}
	if member := w.member(t, "job-bad00001"); member.State != jobFailed || !strings.Contains(member.Reason, w.pool.dir.JobLog("job-bad00001")) {
		t.Fatalf("failed record = %+v, want its log named", member)
	}
	if member := w.member(t, "fleet-a"); !member.TokenAt.Equal(w.clock) {
		t.Fatalf("held record = %+v, want a refreshed token", member)
	}
}

func TestJobsKeepFailsAProvisioningRecordAnEarlierKeepLeft(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	if err := w.pool.save(&jobMember{SchemaVersion: jobsSchema, Name: "job-orphan", State: jobProvisioning, CreatedAt: w.clock}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.pool.keep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if member := w.member(t, "job-orphan"); member.State != jobFailed {
		t.Fatalf("orphaned record = %+v, want failed", member)
	}
}

func TestJobsRunPassesTheMachineAndTheCommandStatusThenReleases(t *testing.T) {
	w := newJobsWorld(t, config.Jobs{})
	w.adopt(t, "fleet-a")
	out := filepath.Join(t.TempDir(), "sprite")

	err := w.pool.run(t.Context(), "replay", 0, []string{"sh", "-c", `printf %s "$SPRITE" > "$1"; exit 7`, "sh", out})
	exited, ok := errors.AsType[*CommandExit](err)
	if !ok || exited.ExitCode() != 7 {
		t.Fatalf("run = %v, want the command's status 7", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "fleet-a" {
		t.Fatalf("SPRITE = %q", got)
	}
	if member := w.member(t, "fleet-a"); member.State != jobReady {
		t.Fatalf("after run = %+v, want ready", member)
	}
}
