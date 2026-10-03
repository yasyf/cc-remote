package orca

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	KeyUp       = "\x1b[A"
	KeyDown     = "\x1b[B"
	selector    = "❯"
	maxMoves    = 4
	idleTimeout = 60 * time.Second
)

var (
	ErrUntrusted = errors.New("the worker asks to trust its checkout, and orca.trust does not list the repository's owner")
	secretToken  = regexp.MustCompile(`sk-[A-Za-z0-9_.\-]*`)
)

type Gate struct {
	Name   string
	Screen *regexp.Regexp
	Choice *regexp.Regexp
	Move   string
	Trust  bool
}

type Startup struct {
	Gates []Gate
	Ready *regexp.Regexp
	Hooks *HookReview
}

type UnknownScreenError struct {
	Steps  []string
	Screen []string
	Cause  error
}

var ClaudeStartup = Startup{
	Gates: []Gate{
		{Name: "theme", Screen: regexp.MustCompile(`Choose the text style`), Choice: regexp.MustCompile(`\bDark mode\s*(✔\s*)?$`), Move: KeyDown},
		{Name: "api-key", Screen: regexp.MustCompile(`Detected a custom API key`), Choice: regexp.MustCompile(`\bYes\b`), Move: KeyUp},
		{Name: "security", Screen: regexp.MustCompile(`Security notes`)},
		{Name: "trust", Screen: regexp.MustCompile(`(?i)trust (the files in )?this folder`), Choice: regexp.MustCompile(`\bYes\b`), Move: KeyDown, Trust: true},
		{Name: "mcp", Screen: regexp.MustCompile(`(?i)MCP servers? found in`), Choice: regexp.MustCompile(`(?i)\benable selected\b`), Move: KeyDown},
		{Name: "bypass", Screen: regexp.MustCompile(`Bypass Permissions mode`), Choice: regexp.MustCompile(`Yes, I accept`), Move: KeyDown},
	},
	Ready: regexp.MustCompile(`API Usage Billing`),
}

func StartupOf(agent Agent) Startup {
	if agent.Kind == AgentClaude {
		return ClaudeStartup
	}
	return Startup{}
}

func (e UnknownScreenError) Error() string {
	return fmt.Sprintf("the worker's screen matched no known startup state after %v: %v; last screen:\n%s", e.Steps, e.Cause, screenText(e.Screen))
}

func (e UnknownScreenError) Unwrap() error { return e.Cause }

func (r Remote) Bootstrap(ctx context.Context, handle string, startup Startup, trusted bool, p Poll) ([]string, error) {
	steps := make([]string, 0, 1)
	var last Screen
	pending := -1
	_, err := poll(ctx, p, func(ctx context.Context) (struct{}, bool, error) {
		screen, err := r.Screen(ctx, handle)
		if err != nil {
			return struct{}{}, false, err
		}
		last = screen
		if startup.Hooks != nil {
			count, found, err := reviewPrompt(screen.Tail)
			if err != nil {
				return struct{}{}, false, err
			}
			if found {
				pending = count
				return struct{}{}, true, completeFrame(screen)
			}
		}
		gate, found := startup.gate(screen.Tail)
		switch {
		case found && gate.Trust && !trusted:
			return struct{}{}, false, ErrUntrusted
		case found:
			if err := r.answer(ctx, handle, gate, screen, p); err != nil {
				return struct{}{}, false, fmt.Errorf("%s: %w", gate.Name, err)
			}
			steps = append(steps, gate.Name)
			return struct{}{}, false, nil
		case startup.Ready != nil && !startup.Ready.MatchString(screenText(screen.Tail)):
			return struct{}{}, false, nil
		}
		wait, err := r.WaitIdle(ctx, handle, idleTimeout)
		blocked := string(bytes.TrimSpace(wait.BlockedReason))
		if err != nil && startup.Hooks != nil && (blocked == reviewBlocked || blocked == trustBlocked) {
			return struct{}{}, false, nil
		}
		return struct{}{}, err == nil, err
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return steps, UnknownScreenError{Steps: steps, Screen: Redact(last.Tail), Cause: err}
	}
	if err != nil || pending < 0 {
		return steps, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	if err := r.reviewHooks(ctx, handle, pending, *startup.Hooks, p); err != nil {
		return steps, fmt.Errorf("hooks: %w", err)
	}
	return append(steps, "hooks"), nil
}

func (r Remote) answer(ctx context.Context, handle string, gate Gate, screen Screen, p Poll) error {
	for moves := 0; gate.Choice != nil && !gate.Choice.MatchString(Selected(screen.Tail)); moves++ {
		before := Selected(screen.Tail)
		if moves == maxMoves {
			return fmt.Errorf("%d moves never selected %s; the selection is %q", maxMoves, gate.Choice, Redact([]string{before})[0])
		}
		if err := r.Key(ctx, handle, gate.Move); err != nil {
			return err
		}
		var err error
		if screen, err = r.until(ctx, handle, p, func(s Screen) bool { return Selected(s.Tail) != before }); err != nil {
			return fmt.Errorf("the selection never moved from %q: %w", Redact([]string{before})[0], err)
		}
	}
	if err := r.Enter(ctx, handle); err != nil {
		return err
	}
	if _, err := r.until(ctx, handle, p, func(s Screen) bool { return !gate.Screen.MatchString(screenText(s.Tail)) }); err != nil {
		return fmt.Errorf("the screen stayed after Enter: %w", err)
	}
	return nil
}

func (r Remote) until(ctx context.Context, handle string, p Poll, done func(Screen) bool) (Screen, error) {
	return poll(ctx, p, func(ctx context.Context) (Screen, bool, error) {
		screen, err := r.Screen(ctx, handle)
		return screen, err == nil && done(screen), err
	})
}

func (s Startup) gate(tail []string) (Gate, bool) {
	text := screenText(tail)
	for _, gate := range s.Gates {
		if gate.Screen.MatchString(text) {
			return gate, true
		}
	}
	return Gate{}, false
}

func Selected(tail []string) string {
	for _, line := range tail {
		for _, marker := range []string{selector, browserSelector} {
			if _, option, ok := strings.Cut(line, marker); ok {
				return strings.TrimSpace(option)
			}
		}
	}
	return ""
}

func Redact(tail []string) []string {
	out := make([]string, len(tail))
	for i, line := range tail {
		out[i] = secretToken.ReplaceAllString(line, "sk-[redacted]")
	}
	return out
}
