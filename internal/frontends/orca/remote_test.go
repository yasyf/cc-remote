package orca_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	env       = "task-a"
	runtimeID = "rt-1"
	scope     = " --environment " + env + " --json"
)

func ok(result string) string {
	return `{"id":"1","ok":true,"result":` + result + `,"_meta":{"runtimeId":"` + runtimeID + `"}}`
}

func TestAddEnvironment(t *testing.T) {
	command := "environment add --name " + env + " --pairing-code " + pairingURL + " --json"
	fake := newFakeOrca(t).on(command, `{"id":"local","ok":true,"result":{"environment":{"id":"env-1","name":"task-a"}},"_meta":{"runtimeId":"local"}}`)
	got, err := orca.NewClient(fake).AddEnvironment(context.Background(), env, pairingURL)
	if err != nil || got != (orca.Environment{ID: "env-1", Name: env}) {
		t.Errorf("AddEnvironment = %+v, %v", got, err)
	}
	failed := newFakeOrca(t).fail(command, `{"id":"local","ok":false,"error":{"code":"invalid_argument","message":"Invalid remote pairing code."}}`,
		errors.New("orca environment add --name task-a --pairing-code "+pairingURL+": exit status 1"))
	_, err = orca.NewClient(failed).AddEnvironment(context.Background(), env, pairingURL)
	if err == nil || !strings.Contains(err.Error(), "invalid_argument: Invalid remote pairing code.") {
		t.Fatalf("AddEnvironment error = %v", err)
	}
	if strings.Contains(err.Error(), "private-pairing-data") {
		t.Errorf("the error leaks the pairing code: %v", err)
	}
	silent := newFakeOrca(t).fail(command, "", &exec.ExitError{})
	if _, err := orca.NewClient(silent).AddEnvironment(context.Background(), env, pairingURL); err == nil || strings.Contains(err.Error(), "private-pairing-data") {
		t.Errorf("AddEnvironment without an envelope = %v", err)
	}
}

func TestStatusRequiresTheRecordedRuntime(t *testing.T) {
	tests := []struct {
		name, out, want string
	}{
		{"ready", ok(`{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1","capabilities":["terminal-prompt-delivery"]}}`), ""},
		{"answered by another runtime", `{"id":"1","ok":true,"result":{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}},"_meta":{"runtimeId":"rt-2"}}`, `answered from runtime "rt-2", not rt-1`},
		{"unreachable", ok(`{"runtime":{"state":"not_running","reachable":false,"runtimeId":null}}`), "reachable false"},
		{"failure envelope", `{"id":"1","ok":false,"error":{"code":"runtime_unavailable","message":"no route"},"_meta":{"runtimeId":null}}`, "runtime_unavailable: no route"},
		{"not ok", `{"id":"1","ok":false,"result":{},"_meta":{"runtimeId":"rt-1"}}`, "without ok: true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t).on("status"+scope, tt.out)
			status, err := orca.NewClient(fake).On(env, runtimeID).Status(context.Background())
			if tt.want == "" {
				if err != nil || status.RuntimeID != runtimeID || status.Capabilities[0] != "terminal-prompt-delivery" {
					t.Errorf("Status = %+v, %v", status, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Status error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAddRepoAndWorktreeAdoptTheExistingCheckout(t *testing.T) {
	root := "/home/agent/app"
	fake := newFakeOrca(t).
		on("repo add --path "+root+scope, ok(`{"repo":{"id":"repo-1","path":"/home/agent/app","displayName":"app"}}`)).
		on("worktree list --repo id:repo-1"+scope, ok(`{"worktrees":[{"id":"repo-1::/home/agent/app/.worktrees/x","repoId":"repo-1","path":"/home/agent/app/.worktrees/x"},{"id":"repo-1::/home/agent/app","repoId":"repo-1","path":"/home/agent/app","branch":"main"}]}`))
	native := orca.NewClient(fake).On(env, runtimeID)
	repo, err := native.AddRepo(context.Background(), root)
	if err != nil || repo.ID != "repo-1" {
		t.Fatalf("AddRepo = %+v, %v", repo, err)
	}
	worktree, err := native.Worktree(context.Background(), repo.ID, root)
	if err != nil || worktree.ID != "repo-1::/home/agent/app" {
		t.Errorf("Worktree = %+v, %v", worktree, err)
	}
	other := newFakeOrca(t).on("repo add --path "+root+scope, ok(`{"repo":{"id":"repo-2","path":"/home/agent/clone"}}`))
	if _, err := orca.NewClient(other).On(env, runtimeID).AddRepo(context.Background(), root); err == nil || !strings.Contains(err.Error(), "not the checkout at "+root) {
		t.Errorf("AddRepo of another path = %v", err)
	}
}

func TestWaitIdleRequiresSatisfied(t *testing.T) {
	command := "terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope
	fake := newFakeOrca(t).
		on(command, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`)).
		on(command, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"permission"}}`))
	native := orca.NewClient(fake).On(env, runtimeID)
	if _, err := native.WaitIdle(context.Background(), "term-1", time.Minute); err != nil {
		t.Errorf("satisfied wait = %v", err)
	}
	if _, err := native.WaitIdle(context.Background(), "term-1", time.Minute); err == nil || !strings.Contains(err.Error(), `blocked "permission"`) {
		t.Errorf("unsatisfied wait = %v", err)
	}
}

func TestPromptSeparatesAcceptanceFromSubmission(t *testing.T) {
	command := "terminal send --terminal term-1 --text do the task --enter --wait-submit 15" + scope
	tests := []struct {
		name      string
		out       string
		submitted bool
		want      string
	}{
		{"turn started", ok(`{"send":{"handle":"term-1","accepted":true,"bytesWritten":11,"prompt":{"requestId":"req-1","stages":["input_accepted","submitted","turn_started"],"provider":"claude","observation":"observed","processIncarnation":"p1"}}}`), true, ""},
		{"accepted only", ok(`{"send":{"handle":"term-1","accepted":true,"bytesWritten":11,"prompt":{"requestId":"req-1","stages":["input_accepted"],"provider":"claude","observation":"timeout","processIncarnation":"p1"}}}`), false, ""},
		{"old host", ok(`{"send":{"handle":"term-1","accepted":true,"bytesWritten":11,"prompt":{"requestId":"unsupported-old-host","stages":["input_accepted","turn_started"],"provider":"old-host","observation":"unsupported","processIncarnation":"unknown"}}}`), false, ""},
		{"refused", ok(`{"send":{"handle":"term-1","accepted":false,"bytesWritten":0,"refusedReason":"terminal exited"}}`), false, "refused the prompt: terminal exited"},
		{"no receipt", ok(`{"send":{"handle":"term-1","accepted":true,"bytesWritten":11}}`), false, "without a prompt receipt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t).on(command, tt.out)
			send, err := orca.NewClient(fake).On(env, runtimeID).Prompt(context.Background(), "term-1", "do the task", 15*time.Second)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("Prompt error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			receipt := send.Receipt(time.Unix(0, 0).UTC())
			if send.Submitted() != tt.submitted || receipt.Submitted != tt.submitted || receipt.Terminal != "term-1" || receipt.RequestID != send.Prompt.RequestID {
				t.Errorf("Submitted = %t, receipt = %+v, want submitted %t", send.Submitted(), receipt, tt.submitted)
			}
		})
	}
}

func TestPromptFailureDoesNotForwardNativeRetryAdvice(t *testing.T) {
	command := "terminal send --terminal term-1 --text do the task --enter --wait-submit 15" + scope
	fake := newFakeOrca(t).on(command, `{"ok":false,"error":{"code":"runtime_error","message":"Transport failed. Re-issue the exact command with --retry-request req-1 --wait-submit 15"},"_meta":{"runtimeId":"rt-1"}}`)
	_, err := orca.NewClient(fake).On(env, runtimeID).Prompt(t.Context(), "term-1", "do the task", 15*time.Second)
	if err == nil || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), "status and read") || strings.Contains(err.Error(), "--retry-request") {
		t.Fatalf("Prompt error = %v", err)
	}
}

func TestRawKeysAndEnterAreSeparateInputs(t *testing.T) {
	fake := newFakeOrca(t).
		on("terminal send --terminal term-1 --text "+orca.KeyDown+scope, ok(`{"send":{"handle":"term-1","accepted":true,"bytesWritten":3}}`)).
		on("terminal send --terminal term-1 --enter"+scope, ok(`{"send":{"handle":"term-1","accepted":false,"bytesWritten":0,"refusedReason":"closed"}}`))
	native := orca.NewClient(fake).On(env, runtimeID)
	if err := native.Key(context.Background(), "term-1", orca.KeyDown); err != nil {
		t.Errorf("Key = %v", err)
	}
	if err := native.Enter(context.Background(), "term-1"); err == nil || !strings.Contains(err.Error(), "refused input: closed") {
		t.Errorf("Enter = %v", err)
	}
}

func TestTerminalRepliesNameTheRequestedTerminal(t *testing.T) {
	read := "terminal read --terminal term-1 --screen" + scope
	key := "terminal send --terminal term-1 --text " + orca.KeyDown + scope
	wait := "terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope
	tests := []struct {
		name, command, out, want string
	}{
		{"screen for another terminal", read, `{"terminal":{"handle":"term-2","status":"running","source":"screen","limited":false,"tail":[]}}`, `orca terminal read on task-a answered for terminal "term-2", not term-1`},
		{"screen without a terminal", read, `{"terminal":{"status":"running","source":"screen","limited":false,"tail":[]}}`, `orca terminal read on task-a answered for terminal "", not term-1`},
		{"key for another terminal", key, `{"send":{"handle":"term-2","accepted":true,"bytesWritten":3}}`, `orca terminal send on task-a answered for terminal "term-2", not term-1`},
		{"blocked wait for another terminal", wait, `{"wait":{"handle":"term-2","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"agent-hooks-review-prompt"}}`, `orca terminal wait on task-a answered for terminal "term-2", not term-1`},
		{"wait for another condition", wait, `{"wait":{"handle":"term-1","condition":"exit","satisfied":true,"status":"exited","exitCode":0}}`, `satisfied "exit", not tui-idle`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			native := orca.NewClient(newFakeOrca(t).on(tt.command, ok(tt.out))).On(env, runtimeID)
			var err error
			switch tt.command {
			case read:
				_, err = native.Screen(context.Background(), "term-1")
			case key:
				err = native.Key(context.Background(), "term-1", orca.KeyDown)
			case wait:
				var got orca.TerminalWait
				got, err = native.WaitIdle(context.Background(), "term-1", time.Minute)
				if got.Handle != "" || got.Satisfied || len(got.BlockedReason) != 0 {
					t.Errorf("WaitIdle returned %+v for a contradictory reply", got)
				}
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
