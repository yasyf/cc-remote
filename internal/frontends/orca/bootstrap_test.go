package orca_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	readScreen = "terminal read --terminal term-1 --screen" + scope
	sendDown   = "terminal send --terminal term-1 --text " + orca.KeyDown + scope
	sendUp     = "terminal send --terminal term-1 --text " + orca.KeyUp + scope
	sendEnter  = "terminal send --terminal term-1 --enter" + scope
	waitIdle   = "terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope
	accepted   = `{"send":{"handle":"term-1","accepted":true,"bytesWritten":1}}`
)

var bootstrapPoll = orca.Poll{Interval: time.Millisecond, Timeout: 2 * time.Second}

func screenOf(t *testing.T, lines ...string) string {
	t.Helper()
	tail, err := json.Marshal(lines)
	if err != nil {
		t.Fatal(err)
	}
	return ok(`{"terminal":{"handle":"term-1","status":"running","source":"screen","tail":` + string(tail) + `}}`)
}

func TestBootstrapWalksClaudesFirstRunScreens(t *testing.T) {
	theme := screenOf(t, "Choose the text style that looks best with your terminal", "❯ 1. Dark mode ✔", "  2. Light mode")
	keyNo := screenOf(t, "Detected a custom API key in your environment", "ANTHROPIC_API_KEY: sk-ant-...wxyz", "Do you want to use this API key?", "  1. Yes", "❯ 2. No (recommended)")
	keyYes := screenOf(t, "Detected a custom API key in your environment", "Do you want to use this API key?", "❯ 1. Yes", "  2. No (recommended)")
	security := screenOf(t, "Security notes:", "Press Enter to continue…")
	trust := screenOf(t, "Do you trust the files in this folder?", "❯ 1. Yes, proceed", "  2. No, exit")
	bypassNo := screenOf(t, "WARNING: Claude Code running in Bypass Permissions mode", "❯ 1. No, exit", "  2. Yes, I accept")
	bypassYes := screenOf(t, "WARNING: Claude Code running in Bypass Permissions mode", "  1. No, exit", "❯ 2. Yes, I accept")
	ready := screenOf(t, "Opus 5.5 (xhigh) · API Usage Billing", "> ", "⏵⏵ bypass permissions on")
	fake := newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendUp, ok(accepted)).on(sendDown, ok(accepted)).
		on(waitIdle, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`))
	for _, screen := range []string{theme, keyNo, keyNo, keyYes, security, security, trust, trust, bypassNo, bypassNo, bypassYes, ready, ready} {
		fake.on(readScreen, screen)
	}
	steps, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.ClaudeStartup, true, bootstrapPoll)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, ","); got != "theme,api-key,security,trust,bypass" {
		t.Errorf("steps = %s", got)
	}
	for command, want := range map[string]int{sendEnter: 5, sendUp: 1, sendDown: 1, waitIdle: 1} {
		if got := fake.called(command); got != want {
			t.Errorf("%q called %d times, want %d", command, got, want)
		}
	}
}

func TestBootstrapRefusesTrustWithoutAuthorization(t *testing.T) {
	fake := newFakeOrca(t).on(readScreen, screenOf(t, "Do you trust the files in this folder?", "❯ 1. Yes, proceed", "  2. No, exit"))
	_, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.ClaudeStartup, false, bootstrapPoll)
	if !errors.Is(err, orca.ErrUntrusted) {
		t.Errorf("Bootstrap = %v, want ErrUntrusted", err)
	}
	if fake.called(sendEnter) != 0 {
		t.Error("an untrusted folder was accepted")
	}
}

func TestBootstrapNamesAnUnknownScreenWithoutTheKey(t *testing.T) {
	fake := newFakeOrca(t).on(readScreen, screenOf(t, "Login failed for sk-ant-api03-secret", "Press any key"))
	_, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.ClaudeStartup, true, orca.Poll{Interval: time.Millisecond, Timeout: 30 * time.Millisecond})
	var unknown orca.UnknownScreenError
	if !errors.As(err, &unknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap = %v, want an UnknownScreenError", err)
	}
	if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "Login failed for sk-[redacted]") {
		t.Errorf("error = %v", err)
	}
}

func TestBootstrapStopsWhenTheSelectionNeverReachesTheChoice(t *testing.T) {
	stuck := screenOf(t, "WARNING: Claude Code running in Bypass Permissions mode", "❯ 1. No, exit", "  2. Yes, I accept")
	moved := screenOf(t, "WARNING: Claude Code running in Bypass Permissions mode", "❯ 3. Something else", "  2. Yes, I accept")
	fake := newFakeOrca(t).on(sendDown, ok(accepted))
	for _, screen := range []string{stuck, moved, stuck, moved, stuck} {
		fake.on(readScreen, screen)
	}
	_, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.ClaudeStartup, true, bootstrapPoll)
	if err == nil || !strings.Contains(err.Error(), "bypass: 4 moves never selected") {
		t.Errorf("Bootstrap = %v", err)
	}
	if fake.called(sendEnter) != 0 {
		t.Error("Enter was sent on an unverified selection")
	}
}

func TestCodexStartupOnlyWaitsForIdle(t *testing.T) {
	fake := newFakeOrca(t).
		on(readScreen, screenOf(t, ">_ OpenAI Codex", "model: gpt-6-astra xhigh")).
		on(waitIdle, ok(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`))
	steps, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.StartupOf(orca.Agent{Kind: orca.AgentCodex}), false, bootstrapPoll)
	if err != nil || len(steps) != 0 || fake.called(waitIdle) != 1 {
		t.Errorf("Bootstrap = %v, %v", steps, err)
	}
}

func TestSelectedAndRedact(t *testing.T) {
	selections := []struct {
		name string
		tail []string
		want string
	}{
		{"claude", []string{"  1. Yes", "❯ 2. No (recommended)"}, "2. No (recommended)"},
		{"codex", []string{"  Folder access", "  1. Trust and continue", "› 2. Quit", "  enter continue · esc quit"}, "2. Quit"},
		{"no menu", []string{"no menu"}, ""},
	}
	for _, tt := range selections {
		if got := orca.Selected(tt.tail); got != tt.want {
			t.Errorf("Selected(%s) = %q, want %q", tt.name, got, tt.want)
		}
	}
	if got := orca.Redact([]string{"key sk-proj-AbC_12.x end"}); got[0] != "key sk-[redacted] end" {
		t.Errorf("Redact = %q", got)
	}
}
