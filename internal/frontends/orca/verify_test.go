package orca_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	worktreesJSON = `{"result":{"worktrees":[
		{"id":"wt-local","displayName":"feature","hostId":"local","repoId":"repo-1","path":"/Users/me/feature","branch":"feature"},
		{"id":"wt-remote","displayName":"feature","hostId":"ssh:ssh-1-abc","repoId":"repo-1","path":"/home/sprite/project","branch":"feature"}
	]}}`
	hostsJSON = `{"result":{"hosts":[
		{"kind":"local","id":"local","name":"this machine"},
		{"kind":"ssh","id":"ssh-1-abc","name":"ws-feature.sprite.cc-remote","connected":true}
	]}}`
	openCall     = "file open README.md --worktree id:wt-remote --json"
	terminalCall = "terminal create --worktree id:wt-remote --title remote-check --command " + orca.ProbeCommand + " --json"
	readCall     = "terminal read --terminal term-7 --json"
	createdJSON  = `{"result":{"terminal":{"handle":"term-7","executionHostId":"ssh:ssh-1-abc"}}}`
)

var fastPoll = orca.Poll{Interval: time.Millisecond, Timeout: time.Second}

func tailJSON(lines ...string) string {
	quoted := make([]string, 0, len(lines))
	for _, l := range lines {
		quoted = append(quoted, `"`+strings.ReplaceAll(l, `"`, `\"`)+`"`)
	}
	return `{"result":{"terminal":{"handle":"term-7","tail":[` + strings.Join(quoted, ",") + `]}}}`
}

func TestParseProbe(t *testing.T) {
	echoed := "% " + orca.ProbeCommand
	tests := []struct {
		name    string
		tail    []string
		want    orca.Probe
		wantErr error
		errText string
	}{
		{
			name: "success after the echoed command",
			tail: []string{echoed, "remote-check ws-feature feature /home/sprite/project", "remote-check-exit 0"},
			want: orca.Probe{Hostname: "ws-feature", Branch: "feature", Cwd: "/home/sprite/project"},
		},
		{
			name: "carriage returns from the terminal stream",
			tail: []string{"remote-check ws-feature main /work\r", "remote-check-exit 0\r"},
			want: orca.Probe{Hostname: "ws-feature", Branch: "main", Cwd: "/work"},
		},
		{
			name: "checkout path with spaces",
			tail: []string{"remote-check ws-feature main /work/my project", "remote-check-exit 0"},
			want: orca.Probe{Hostname: "ws-feature", Branch: "main", Cwd: "/work/my project"},
		},
		{name: "only the echoed command so far", tail: []string{echoed}, wantErr: orca.ErrProbeRunning},
		{name: "empty tail", tail: nil, wantErr: orca.ErrProbeRunning},
		{
			name:    "git failure exits non-zero",
			tail:    []string{echoed, "fatal: not a git repository", "remote-check-exit 128"},
			wantErr: orca.ProbeExitError{Status: 128},
		},
		{
			name:    "exit zero without a result line",
			tail:    []string{"remote-check-exit 0"},
			errText: "remote-check exited 0 without its result line",
		},
		{
			name:    "relative cwd is not a result line",
			tail:    []string{"remote-check ws main work", "remote-check-exit 0"},
			errText: "without its result line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orca.ParseProbe(tt.tail)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseProbe() error = %v, want %v", err, tt.wantErr)
				}
			case tt.errText != "":
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("ParseProbe() error = %v, want %q", err, tt.errText)
				}
			case err != nil:
				t.Fatalf("ParseProbe() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseProbe() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProbeCommand(t *testing.T) {
	tests := []struct {
		name       string
		launcher   string
		cliStatus  int
		gitStatus  int
		wantStatus int
		wantCall   bool
	}{
		{name: "missing launcher", wantStatus: 1},
		{name: "different launcher", launcher: "other", wantStatus: 1},
		{name: "relay command fails", launcher: "relay", cliStatus: 17, wantStatus: 17, wantCall: true},
		{name: "git command fails", launcher: "relay", gitStatus: 128, wantStatus: 128, wantCall: true},
		{name: "relay round trip succeeds", launcher: "relay", wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "agent home")
			relay := filepath.Join(home, ".orca-relay", "bin")
			bin := filepath.Join(root, "bin")
			checkout := filepath.Join(root, "my project")
			for _, dir := range []string{relay, bin, checkout} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeScript := func(dir, name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeScript(bin, "git", "printf 'feature\\n'\nexit "+strconv.Itoa(tt.gitStatus))
			writeScript(bin, "hostname", "printf 'remote-fixture\\n'")
			if tt.launcher != "" {
				dir := relay
				if tt.launcher == "other" {
					dir = bin
				}
				writeScript(dir, "orca", `printf '%s\n' "$@" >"$HOME/cli-args"`+"\nprintf '{\"result\":{\"worktree\":{\"id\":\"wt-remote\"}}}\\n'\nexit "+strconv.Itoa(tt.cliStatus))
			}
			cmd := exec.Command("/bin/sh", "-c", orca.ProbeCommand)
			cmd.Dir = checkout
			cmd.Env = []string{"HOME=" + home, "PATH=" + relay + string(os.PathListSeparator) + bin, "PWD=" + checkout}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("probe shell: %v: %s", err, output)
			}
			got, err := orca.ParseProbe(strings.Split(string(output), "\n"))
			if tt.wantStatus != 0 {
				if !errors.Is(err, orca.ProbeExitError{Status: tt.wantStatus}) {
					t.Fatalf("ParseProbe() error = %v, want exit %d; output: %s", err, tt.wantStatus, output)
				}
				if strings.Contains(string(output), "remote-check remote-fixture") {
					t.Fatalf("failed probe printed a success marker: %s", output)
				}
			} else {
				want := orca.Probe{Hostname: "remote-fixture", Branch: "feature", Cwd: checkout}
				if err != nil || got != want {
					t.Fatalf("ParseProbe() = %+v, %v; want %+v", got, err, want)
				}
			}
			args, err := os.ReadFile(filepath.Join(home, "cli-args"))
			if tt.wantCall {
				if err != nil || string(args) != "worktree\ncurrent\n--json\n" {
					t.Fatalf("relay call = %q, %v", args, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected CLI invocation: %q, %v", args, err)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	opts := orca.VerifyOptions{Workspace: "feature", Open: "README.md", LocalHostname: "my-mac"}
	t.Run("proves the probe exited 0 on the remote host and keeps its terminal", func(t *testing.T) {
		fake := newFakeOrca(t).
			on("worktree list --limit 10000 --json", worktreesJSON).
			on("host list --json", hostsJSON).
			on(openCall, `{"result":{}}`).
			on(terminalCall, createdJSON).
			on(readCall, tailJSON("% branch=...")).
			on(readCall, tailJSON("remote-check ws-feature feature /home/sprite/project", "remote-check-exit 0"))
		got, err := orca.NewClient(fake).Verify(context.Background(), opts, fastPoll)
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		want := orca.Verification{
			Worktree:      "wt-remote",
			Host:          "ssh:ssh-1-abc",
			Target:        "ws-feature.sprite.cc-remote",
			Connected:     true,
			Hostname:      "ws-feature",
			Branch:        "feature",
			Cwd:           "/home/sprite/project",
			Editor:        "README.md opened",
			ProbeTerminal: "term-7",
		}
		if got != want {
			t.Errorf("Verify() = %+v, want %+v", got, want)
		}
		if n := fake.called(readCall); n != 2 {
			t.Errorf("terminal read calls = %d, want 2", n)
		}
		for _, c := range fake.calls {
			if strings.HasPrefix(c, "terminal close") || strings.HasPrefix(c, "worktree rm") {
				t.Errorf("verify ran %q; the probe terminal and workspace belong to the task", c)
			}
		}
	})

	failures := []struct {
		name  string
		setup func(*fakeOrca)
		want  string
	}{
		{
			name:  "no SSH workspace",
			setup: func(f *fakeOrca) { f.on("worktree list --limit 10000 --json", `{"result":{"worktrees":[]}}`) },
			want:  "no SSH workspace named feature in Orca",
		},
		{
			name: "disconnected target",
			setup: func(f *fakeOrca) {
				f.on("worktree list --limit 10000 --json", worktreesJSON).
					on("host list --json", strings.Replace(hostsJSON, `"connected":true`, `"connected":false`, 1))
			},
			want: "SSH target ssh:ssh-1-abc (ws-feature.sprite.cc-remote) is not connected",
		},
		{
			name: "terminal started on the Mac",
			setup: func(f *fakeOrca) {
				f.on("worktree list --limit 10000 --json", worktreesJSON).
					on("host list --json", hostsJSON).
					on(openCall, `{"result":{}}`).
					on(terminalCall, strings.Replace(createdJSON, "ssh:ssh-1-abc", "local", 1))
			},
			want: `the remote-check terminal term-7 started on "local", not ssh:ssh-1-abc`,
		},
		{
			name: "probe exits non-zero",
			setup: func(f *fakeOrca) {
				f.on("worktree list --limit 10000 --json", worktreesJSON).
					on("host list --json", hostsJSON).
					on(openCall, `{"result":{}}`).
					on(terminalCall, createdJSON).
					on(readCall, tailJSON("remote-check-exit 1"))
			},
			want: "terminal term-7: remote-check exited 1",
		},
		{
			name: "probe ran on this Mac",
			setup: func(f *fakeOrca) {
				f.on("worktree list --limit 10000 --json", worktreesJSON).
					on("host list --json", hostsJSON).
					on(openCall, `{"result":{}}`).
					on(terminalCall, createdJSON).
					on(readCall, tailJSON("remote-check my-mac feature /Users/me/feature", "remote-check-exit 0"))
			},
			want: "remote-check in terminal term-7 ran on this Mac, not ssh:ssh-1-abc",
		},
		{
			name: "probe never exits",
			setup: func(f *fakeOrca) {
				f.on("worktree list --limit 10000 --json", worktreesJSON).
					on("host list --json", hostsJSON).
					on(openCall, `{"result":{}}`).
					on(terminalCall, createdJSON).
					on(readCall, tailJSON("% branch=..."))
			},
			want: "the remote-check command in terminal term-7 never exited",
		},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t)
			tt.setup(fake)
			wait := fastPoll
			wait.Timeout = 20 * time.Millisecond
			_, err := orca.NewClient(fake).Verify(context.Background(), opts, wait)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Verify() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestVerifyRejectsAmbiguousName(t *testing.T) {
	twoRepos := strings.Replace(worktreesJSON, `"hostId":"local"`, `"hostId":"ssh:ssh-2"`, 1)
	fake := newFakeOrca(t).on("worktree list --limit 10000 --json", twoRepos)
	_, err := orca.NewClient(fake).Verify(context.Background(), orca.VerifyOptions{Workspace: "feature", Open: "README.md"}, fastPoll)
	if err == nil || !strings.Contains(err.Error(), "2 SSH workspaces named feature in Orca; pass --repo") {
		t.Errorf("Verify() error = %v, want the ambiguity error", err)
	}
}

func TestGone(t *testing.T) {
	tests := []struct {
		name      string
		worktrees string
		hosts     string
		want      string
	}{
		{name: "workspace still listed", worktrees: worktreesJSON, hosts: hostsJSON, want: "orca still lists a workspace on ssh:ssh-1-abc"},
		{name: "target still listed", worktrees: `{"result":{"worktrees":[]}}`, hosts: hostsJSON, want: "orca still lists SSH target ssh:ssh-1-abc"},
		{name: "gone", worktrees: `{"result":{"worktrees":[]}}`, hosts: `{"result":{"hosts":[{"kind":"local","id":"local"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t).on("worktree list --limit 10000 --json", tt.worktrees).on("host list --json", tt.hosts)
			got, err := orca.NewClient(fake).Gone(context.Background(), "ssh:ssh-1-abc")
			if tt.want != "" {
				if err == nil || err.Error() != tt.want {
					t.Errorf("Gone() error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil || got != (orca.Removal{Host: "ssh:ssh-1-abc", Gone: true}) {
				t.Errorf("Gone() = %+v, %v", got, err)
			}
		})
	}
}
