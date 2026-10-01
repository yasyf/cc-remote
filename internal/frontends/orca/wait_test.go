package orca_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	statusReady  = `{"result":{"runtime":{"state":"ready"},"app":{"desktopWindowStatus":"available"}}}`
	doctorCall   = "vm recipe doctor sprites-agents-ssh --repo-path %s --json"
	doctorPassed = `{"recipeId":"sprites-agents-ssh","ok":true,"checks":[
		{"id":"recipe.exists","status":"pass","message":"Found recipe \"Sprites agents over SSH (default).\""},
		{"id":"recipe.create","status":"warn","message":"Command is not a repo-relative path: cc-remote"},
		{"id":"recipe.destroy","status":"warn","message":"Command is not a repo-relative path: cc-remote"}
	]}`
)

type waitFixture struct {
	checkout  string
	preflight orca.Preflight
}

func newWaitFixture(t *testing.T) waitFixture {
	t.Helper()
	checkout := t.TempDir()
	orcaYAML, err := orca.MergeYAML(nil, orca.DefaultLifecycle(), []orca.Recipe{defaultSprites})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "orca.yaml"), orcaYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	return waitFixture{
		checkout: checkout,
		preflight: orca.Preflight{
			Recipe:    defaultSprites,
			Lifecycle: orca.DefaultLifecycle(),
			Workspace: "feature",
			Checkout:  checkout,
		},
	}
}

func (f waitFixture) reposJSON() string {
	return `{"result":{"repos":[{"id":"repo-0","path":"/elsewhere","displayName":"other"},{"id":"repo-1","path":"` + f.checkout + `","displayName":"project"}]}}`
}

func (f waitFixture) doctorCall() string {
	return strings.Replace(doctorCall, "%s", f.checkout, 1)
}

func TestWaitWaitsForOrcaToListTheSSHWorkspace(t *testing.T) {
	f := newWaitFixture(t)
	fake := newFakeOrca(t).
		on("status --json", statusReady).
		on("repo list --json", f.reposJSON()).
		on(f.doctorCall(), doctorPassed).
		on("worktree list --limit 10000 --json", `{"result":{"worktrees":[]}}`).
		on("worktree list --limit 10000 --json", worktreesJSON)
	got, err := orca.NewClient(fake).Wait(context.Background(), f.preflight, fastPoll)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	want := orca.SSHWorkspace{
		Recipe:    "sprites-agents-ssh",
		Name:      "Sprites agents over SSH (default)",
		Checkout:  f.checkout,
		Workspace: "feature",
		Worktree:  "wt-remote",
		Host:      "ssh:ssh-1-abc",
		Path:      "/home/sprite/project",
		Branch:    "feature",
	}
	if got != want {
		t.Errorf("Wait() = %+v, want %+v", got, want)
	}
	if n := fake.called("worktree list --limit 10000 --json"); n != 2 {
		t.Errorf("worktree list calls = %d, want 2", n)
	}
}

func TestWaitPreflightFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, waitFixture) (orca.Preflight, *fakeOrca)
		wantErr error
		want    string
	}{
		{
			name: "runtime not ready",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				return f.preflight, newFakeOrca(t).on("status --json", strings.Replace(statusReady, `"ready"`, `"starting"`, 1))
			},
			wantErr: orca.ErrNotReady,
		},
		{
			name: "no window",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				return f.preflight, newFakeOrca(t).on("status --json", strings.Replace(statusReady, `"available"`, `"hidden"`, 1))
			},
			wantErr: orca.ErrNotReady,
		},
		{
			name: "checkout not in Orca",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				return f.preflight, newFakeOrca(t).on("status --json", statusReady).on("repo list --json", `{"result":{"repos":[]}}`)
			},
			want: "no Orca repo at " + "%CHECKOUT%" + "; add the checkout to Orca or pass --repo",
		},
		{
			name: "unknown repo id",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				p := f.preflight
				p.RepoID = "repo-9"
				return p, newFakeOrca(t).on("status --json", statusReady).on("repo list --json", f.reposJSON())
			},
			want: "no Orca repo has id repo-9",
		},
		{
			name: "recipe missing from orca.yaml",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				p := f.preflight
				p.Recipe = orca.Recipe{Provider: "namespace", Profile: "stack", Name: "Namespace stack"}
				return p, newFakeOrca(t).on("status --json", statusReady).on("repo list --json", f.reposJSON())
			},
			wantErr: orca.ErrMissingRecipe,
		},
		{
			name: "orca.yaml written for another config",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				p := f.preflight
				p.Lifecycle = orca.Lifecycle{Binary: "cc-remote", Config: "remote.yaml"}
				return p, newFakeOrca(t).on("status --json", statusReady).on("repo list --json", f.reposJSON())
			},
			wantErr: orca.ErrStaleRecipe,
		},
		{
			name: "doctor fails a check",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				failed := `{"recipeId":"sprites-agents-ssh","checks":[{"id":"recipe.exists","status":"pass","message":"ok"},{"id":"command.create","status":"fail","message":"cc-remote is not on PATH"}]}`
				return f.preflight, newFakeOrca(t).
					on("status --json", statusReady).
					on("repo list --json", f.reposJSON()).
					fail(f.doctorCall(), failed, errors.New("exit status 1"))
			},
			want: "orca vm recipe doctor sprites-agents-ssh: doctor fail: command.create: cc-remote is not on PATH",
		},
		{
			name: "doctor warning beyond the binary's path",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				warned := `{"recipeId":"sprites-agents-ssh","ok":true,"checks":[{"id":"recipe.exists","status":"pass","message":"ok"},{"id":"recipe.suspend_resume_pairing","status":"warn","message":"Recipe defines only one of suspend/resume."}]}`
				return f.preflight, newFakeOrca(t).
					on("status --json", statusReady).
					on("repo list --json", f.reposJSON()).
					on(f.doctorCall(), warned)
			},
			want: "orca vm recipe doctor sprites-agents-ssh: doctor warn: recipe.suspend_resume_pairing: Recipe defines only one of suspend/resume.",
		},
		{
			name: "repo-relative binary is not executable",
			mutate: func(t *testing.T, f waitFixture) (orca.Preflight, *fakeOrca) {
				p := f.preflight
				p.Lifecycle = orca.Lifecycle{Binary: "./bin/cc-remote"}
				orcaYAML, err := orca.MergeYAML(nil, p.Lifecycle, []orca.Recipe{defaultSprites})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.checkout, "orca.yaml"), orcaYAML, 0o600); err != nil {
					t.Fatal(err)
				}
				warned := `{"recipeId":"sprites-agents-ssh","ok":true,"checks":[{"id":"recipe.exists","status":"pass","message":"ok"},{"id":"recipe.create","status":"warn","message":"Command exists but is not executable: ./bin/cc-remote"}]}`
				return p, newFakeOrca(t).
					on("status --json", statusReady).
					on("repo list --json", f.reposJSON()).
					on(f.doctorCall(), warned)
			},
			want: "orca vm recipe doctor sprites-agents-ssh: doctor warn: recipe.create: Command exists but is not executable: ./bin/cc-remote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWaitFixture(t)
			p, fake := tt.mutate(t, f)
			_, err := orca.NewClient(fake).Wait(context.Background(), p, fastPoll)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("Wait() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			want := strings.NewReplacer("%CHECKOUT%", f.checkout).Replace(tt.want)
			if err == nil || err.Error() != want {
				t.Errorf("Wait() error = %v, want %q", err, want)
			}
		})
	}
}

func TestWaitTimesOut(t *testing.T) {
	f := newWaitFixture(t)
	fake := newFakeOrca(t).
		on("status --json", statusReady).
		on("repo list --json", f.reposJSON()).
		on(f.doctorCall(), doctorPassed).
		on("worktree list --limit 10000 --json", `{"result":{"worktrees":[]}}`)
	wait := orca.Poll{Interval: time.Millisecond, Timeout: 20 * time.Millisecond}
	_, err := orca.NewClient(fake).Wait(context.Background(), f.preflight, wait)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no SSH workspace named feature after 20ms") {
		t.Errorf("Wait() error = %v, want a timeout naming the workspace", err)
	}
}
