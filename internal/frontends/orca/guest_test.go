package orca_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func TestLocalTargetDropsOnlyTheEnvironment(t *testing.T) {
	ready := `{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}}`
	fake := newFakeOrca(t).
		on("status --json", ok(ready)).
		on("status --json", `{"id":"1","ok":true,"result":`+ready+`,"_meta":{"runtimeId":"rt-2"}}`)
	native := orca.NewClient(fake).Local(runtimeID)
	if _, err := native.Status(context.Background()); err != nil {
		t.Fatalf("local Status = %v", err)
	}
	if _, err := native.Status(context.Background()); err == nil || !strings.Contains(err.Error(), `answered from runtime "rt-2", not rt-1`) {
		t.Errorf("local Status from another runtime = %v", err)
	}
	if !slices.Equal(fake.calls, []string{"status --json", "status --json"}) {
		t.Errorf("calls = %q", fake.calls)
	}
}

func TestBootstrapIsTheSameStateMachineOnEitherTarget(t *testing.T) {
	run := func(t *testing.T, suffix string, target func(orca.Client) orca.Remote) ([]string, []string) {
		var steps, calls []string
		synctest.Test(t, func(t *testing.T) {
			theme := screenOf(t, "Choose the text style that looks best with your terminal", "❯ 1. Dark mode ✔", "  2. Light mode")
			keyNo := screenOf(t, "Detected a custom API key in your environment", "Do you want to use this API key?", "  1. Yes", "❯ 2. No (recommended)")
			keyYes := screenOf(t, "Detected a custom API key in your environment", "Do you want to use this API key?", "❯ 1. Yes", "  2. No (recommended)")
			trust := screenOf(t, "Do you trust the files in this folder?", "❯ 1. Yes, proceed", "  2. No, exit")
			ready := screenOf(t, "Opus 5.5 (xhigh) · API Usage Billing", "> ")
			read := "terminal read --terminal term-1 --screen" + suffix
			fake := newFakeOrca(t).
				on("terminal send --terminal term-1 --enter"+suffix, ok(accepted)).
				on("terminal send --terminal term-1 --text "+orca.KeyUp+suffix, ok(accepted)).
				on("terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000"+suffix, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`))
			for _, screen := range []string{theme, keyNo, keyNo, keyYes, trust, trust, ready, ready} {
				fake.on(read, screen)
			}
			var err error
			if steps, err = target(orca.NewClient(fake)).Bootstrap(context.Background(), "term-1", orca.ClaudeStartup, true, bootstrapPoll); err != nil {
				t.Fatal(err)
			}
			for _, call := range fake.calls {
				calls = append(calls, strings.TrimSuffix(call, suffix))
			}
		})
		return steps, calls
	}
	remoteSteps, remoteCalls := run(t, scope, func(c orca.Client) orca.Remote { return c.On(env, runtimeID) })
	localSteps, localCalls := run(t, " --json", func(c orca.Client) orca.Remote { return c.Local(runtimeID) })
	if !slices.Equal(remoteSteps, []string{"theme", "api-key", "trust"}) || !slices.Equal(localSteps, remoteSteps) {
		t.Errorf("steps: remote %q, local %q", remoteSteps, localSteps)
	}
	if !slices.Equal(localCalls, remoteCalls) {
		t.Errorf("local calls\n%q\ndiffer from remote calls\n%q", localCalls, remoteCalls)
	}
}

func TestNewGuestCarriesOnlyNonsecretMetadata(t *testing.T) {
	prior := orca.Pregrant{Project: grantProject, Hooks: grantHooks(5)}
	grant := orca.HookGrant{Captain: captainPins, Codex: "0.159.2", Project: grantProject, Exec: func(context.Context, []string) ([]byte, error) { return nil, nil }}
	agent := orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh", Tier: "fast"}
	tests := []struct {
		name    string
		agent   orca.Agent
		startup orca.Startup
		keys    []string
	}{
		{"granted codex", agent, orca.GrantedCodexStartup(grant, prior), []string{"agent", "cli", "grant", "interval", "platform", "runtimeId", "schema", "terminal", "timeout", "trusted", "version"}},
		{"reviewed codex", agent, orca.CodexStartup(orca.HookReview{Captain: captainPins, Exec: grant.Exec}), []string{"agent", "cli", "interval", "platform", "review", "runtimeId", "schema", "terminal", "timeout", "trusted", "version"}},
		{"claude", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, orca.ClaudeStartup, []string{"agent", "cli", "interval", "platform", "runtimeId", "schema", "terminal", "timeout", "trusted", "version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guest := orca.NewGuest("linux/amd64", "rt-1", "term-1", "tools/orca/squashfs-root/"+orca.GuestCLI, tt.agent, tt.startup, true, bootstrapPoll)
			raw, err := json.Marshal(guest)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(fields)); !slices.Equal(got, tt.keys) {
				t.Errorf("descriptor fields = %q, want %q", got, tt.keys)
			}
			for _, private := range []string{leaked, "sk-", tt.agent.Model, "xhigh", "fast", "pairing", "capability"} {
				if strings.Contains(string(raw), private) {
					t.Errorf("the descriptor carries %q: %s", private, raw)
				}
			}
			var decoded orca.Guest
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&decoded); err != nil {
				t.Fatal(err)
			}
			if again, _ := json.Marshal(decoded); !bytes.Equal(again, raw) {
				t.Errorf("the descriptor does not round-trip:\n%s\n%s", again, raw)
			}
		})
	}
}

func TestParseGuestResultKeepsTheTypedFailure(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		steps []string
		is    error
		want  string
	}{
		{"ready", nil, []string{"theme", "bypass"}, nil, ""},
		{"untrusted", orca.ErrUntrusted, nil, orca.ErrUntrusted, orca.ErrUntrusted.Error()},
		{"pregrant prompt", errors.Join(errors.New("trust"), orca.ErrPregrantPrompt), []string{"hooks.granted"}, orca.ErrPregrantPrompt, "pregranted Codex worker"},
		{"other failure", errors.New("terminal term-1 on local was not tui-idle"), []string{"theme"}, nil, "was not tui-idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(orca.GuestOutcome(tt.steps, tt.err))
			if err != nil {
				t.Fatal(err)
			}
			steps, err := orca.ParseGuestResult(raw)
			if !slices.Equal(steps, tt.steps) {
				t.Errorf("steps = %q, want %q", steps, tt.steps)
			}
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("ParseGuestResult = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "the guest bootstrap helper: ")):
				t.Fatalf("ParseGuestResult = %v, want %q", err, tt.want)
			case tt.is != nil && !errors.Is(err, tt.is):
				t.Errorf("ParseGuestResult = %v, not %v", err, tt.is)
			}
		})
	}
	for _, out := range []string{"", "not json", `{"schema":2,"steps":[]}`, `{"schema":1,"steps":[],"extra":1}`, `{"schema":1,"steps":[]}{}`, `{"schema":1,"steps":[],"kind":"fallback","error":"x"}`, `{"schema":1,"steps":[],"kind":"failed"}`} {
		if _, err := orca.ParseGuestResult([]byte(out)); err == nil {
			t.Errorf("ParseGuestResult accepted %q", out)
		}
	}
}
