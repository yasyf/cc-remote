package orca_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/version"
)

const fakeGuestCLI = `#!/usr/bin/env python3
import json
import os
import sys

here = os.path.dirname(os.path.realpath(__file__))
with open(os.path.join(here, "scenario.json")) as source:
    scenario = json.load(source)
state_path = os.path.join(here, "state.json")
state = {}
if os.path.exists(state_path):
    with open(state_path) as source:
        state = json.load(source)
command = " ".join(sys.argv[1:])
routing = sorted(name for name in ("ORCA_PAIRING_CODE", "ORCA_REMOTE_PAIRING", "ORCA_ENVIRONMENT") if name in os.environ)
with open(os.path.join(here, "calls.jsonl"), "a") as log:
    log.write(json.dumps({"command": command, "userData": os.environ.get("ORCA_USER_DATA_PATH"), "routing": routing}) + "\n")
replies = scenario.get(command, [])
if not replies:
    print(json.dumps({"ok": False, "error": {"code": "unexpected", "message": command}, "_meta": {"runtimeId": None}}))
    sys.exit(1)
count = state.get(command, 0)
state[command] = count + 1
with open(state_path, "w") as sink:
    json.dump(state, sink)
print(replies[min(count, len(replies) - 1)])
`

const guestCLIPath = "tools/orca/squashfs-root/" + orca.GuestCLI

type guestHome struct {
	home     string
	bin      string
	scenario map[string][]string
}

type guestCall struct {
	Command  string   `json:"command"`
	UserData string   `json:"userData"`
	Routing  []string `json:"routing"`
}

func newGuestHome(t *testing.T) *guestHome {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &guestHome{home: home, bin: filepath.Join(home, filepath.Dir(guestCLIPath)), scenario: map[string][]string{}}
	if err := os.MkdirAll(filepath.Join(home, ".cc-remote/orca/user-data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, guestCLIPath), []byte(fakeGuestCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	h.on("status --json", ok(`{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}}`))
	t.Setenv("ORCA_PAIRING_CODE", "orca://pair?code=private")
	t.Setenv("ORCA_ENVIRONMENT", "task-a")
	t.Setenv("ORCA_USER_DATA_PATH", "/elsewhere")
	return h
}

func (h *guestHome) on(command string, replies ...string) {
	h.scenario[command] = append(h.scenario[command], replies...)
}

func (h *guestHome) run(t *testing.T, guest orca.Guest, local func(context.Context, []string) ([]byte, error)) ([]string, error) {
	t.Helper()
	raw, err := json.Marshal(h.scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.bin, "scenario.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return guest.Run(context.Background(), h.home, local)
}

func (h *guestHome) calls(t *testing.T) []guestCall {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.bin, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []guestCall
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var call guestCall
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func guestFor(agent string, startup orca.Startup) orca.Guest {
	return orca.NewGuest(runtime.GOOS+"/"+runtime.GOARCH, "rt-1", "term-1", guestCLIPath, orca.Agent{Kind: agent}, startup, true, bootstrapPoll)
}

func noLocalExec(t *testing.T) func(context.Context, []string) ([]byte, error) {
	return func(context.Context, []string) ([]byte, error) {
		t.Error("a hook check ran for a worker that has none")
		return nil, errors.New("unexpected")
	}
}

const (
	localRead = "terminal read --terminal term-1 --screen --json"
	localWait = "terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000 --json"
)

func TestGuestRunDrivesTheOwnedLocalRuntime(t *testing.T) {
	h := newGuestHome(t)
	h.on(localRead, screenOf(t, "Opus 5.5 (xhigh) · API Usage Billing", "> "))
	h.on(localWait, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`))
	steps, err := h.run(t, guestFor(orca.AgentClaude, orca.ClaudeStartup), noLocalExec(t))
	if err != nil || len(steps) != 0 {
		t.Fatalf("Run = %q, %v", steps, err)
	}
	calls := h.calls(t)
	commands := make([]string, 0, len(calls))
	for _, call := range calls {
		commands = append(commands, call.Command)
		if call.UserData != filepath.Join(h.home, ".cc-remote/orca/user-data") || len(call.Routing) != 0 {
			t.Errorf("%s ran with user data %q and routing %q", call.Command, call.UserData, call.Routing)
		}
	}
	if !slices.Equal(commands, []string{"status --json", localRead, localWait}) {
		t.Errorf("commands = %q", commands)
	}
}

func TestGuestRunRefusesBeforeAnyOrcaCall(t *testing.T) {
	review := orca.CodexStartup(orca.HookReview{Captain: captainPins})
	tests := []struct {
		name  string
		edit  func(*orca.Guest, *guestHome)
		want  string
		calls []string
	}{
		{"another release", func(g *orca.Guest, _ *guestHome) { g.Version = "0.0.1" }, "the guest helper is cc-remote " + version.String() + ", and the Mac runs cc-remote 0.0.1", nil},
		{"another platform", func(g *orca.Guest, _ *guestHome) { g.Platform = "darwin/arm64" }, "and the descriptor names darwin/arm64", nil},
		{"another schema", func(g *orca.Guest, _ *guestHome) { g.Schema = 2 }, "schema 2", nil},
		{"unknown agent", func(g *orca.Guest, _ *guestHome) { g.Agent = "gemini" }, `agent "gemini"`, nil},
		{"claude with a hook review", func(g *orca.Guest, _ *guestHome) { g.Review = &orca.GuestReview{Captain: captainPins} }, "gives claude a Codex hook check", nil},
		{"codex without a hook check", func(g *orca.Guest, _ *guestHome) { *g = guestFor(orca.AgentCodex, orca.Startup{}) }, "neither or both", nil},
		{"codex with both hook checks", func(g *orca.Guest, _ *guestHome) {
			*g = guestFor(orca.AgentCodex, review)
			g.Grant = &orca.GuestGrant{Captain: captainPins}
		}, "neither or both", nil},
		{"no terminal", func(g *orca.Guest, _ *guestHome) { g.Terminal = "" }, "no runtime or terminal", nil},
		{"an unbounded poll", func(g *orca.Guest, _ *guestHome) { g.Timeout = 0 }, "polls every", nil},
		{"a CLI outside the home", func(g *orca.Guest, _ *guestHome) { g.CLI = "../" + guestCLIPath }, "not a clean path under the guest home", nil},
		{"a missing CLI", func(g *orca.Guest, _ *guestHome) { g.CLI = "tools/other/orca-ide" }, "is not an executable regular file", nil},
		{"missing user data", func(_ *orca.Guest, h *guestHome) {
			if err := os.RemoveAll(filepath.Join(h.home, ".cc-remote")); err != nil {
				t.Fatal(err)
			}
		}, "the guest Orca user data", nil},
		{"another runtime", func(_ *orca.Guest, h *guestHome) {
			h.scenario["status --json"] = []string{`{"id":"1","ok":true,"result":{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}},"_meta":{"runtimeId":"rt-2"}}`}
		}, `answered from runtime "rt-2", not rt-1`, []string{"status --json"}},
		{"another terminal", func(_ *orca.Guest, h *guestHome) {
			h.on(localRead, ok(`{"terminal":{"handle":"term-2","status":"running","source":"screen","tail":["> "]}}`))
		}, `answered for terminal "term-2", not term-1`, []string{"status --json", localRead}},
		{"an idle wait that is not satisfied", func(_ *orca.Guest, h *guestHome) {
			h.on(localRead, screenOf(t, "Opus 5.5 (xhigh) · API Usage Billing", "> "))
			h.on(localWait, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"busy"}}`))
		}, "was not tui-idle", []string{"status --json", localRead, localWait}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGuestHome(t)
			guest := guestFor(orca.AgentClaude, orca.ClaudeStartup)
			tt.edit(&guest, h)
			_, err := h.run(t, guest, noLocalExec(t))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want %q", err, tt.want)
			}
			calls := h.calls(t)
			commands := make([]string, 0, len(calls))
			for _, call := range calls {
				commands = append(commands, call.Command)
			}
			if !slices.Equal(commands, tt.calls) {
				t.Errorf("commands = %q, want %q", commands, tt.calls)
			}
		})
	}
}

func TestGuestRunKeepsTheFinalGrantVerification(t *testing.T) {
	prior := orca.Pregrant{Project: grantProject, Hooks: grantHooks(5)}
	tests := []struct {
		name  string
		final string
		want  error
		waits int
	}{
		{"verified before the idle wait", grantDoc(t, orca.GrantFinal, nil), nil, 1},
		{"a refused final grant", refusedDoc(orca.GrantFinal, "untrusted-hook", "Stop", 1, ""), orca.GrantRefusal{Mode: orca.GrantFinal, Reason: "untrusted-hook", Event: "Stop", Record: 1}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGuestHome(t)
			h.on(localRead, frameOf(t, "screen", false, false, codexReady))
			h.on(localWait, ok(idleWait))
			execs := 0
			local := func(_ context.Context, argv []string) ([]byte, error) {
				execs++
				if argv[0] != "python3" || argv[3] != string(orca.GrantFinal) {
					t.Errorf("the local hook check ran %q", argv[:4])
				}
				return []byte(tt.final), nil
			}
			grant := orca.HookGrant{Captain: captainPins, Codex: "0.159.2", Project: grantProject}
			steps, err := h.run(t, guestFor(orca.AgentCodex, orca.GrantedCodexStartup(grant, prior)), local)
			switch {
			case tt.want == nil && (err != nil || !slices.Equal(steps, []string{"hooks.granted"})):
				t.Fatalf("Run = %q, %v", steps, err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Fatalf("Run = %v, want %v", err, tt.want)
			}
			waits := 0
			for _, call := range h.calls(t) {
				if call.Command == localWait {
					waits++
				}
			}
			if execs != 1 || waits != tt.waits {
				t.Errorf("final verifications = %d, idle waits = %d; want 1 and %d", execs, waits, tt.waits)
			}
		})
	}
}
