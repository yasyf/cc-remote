package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	LocalRuntime      = "local"
	screenUnavailable = "screen-unavailable"
	tuiIdle           = "tui-idle"
	turnStarted       = "turn_started"
	oldHost           = "old-host"
)

type Environment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RuntimeStatus struct {
	State        string   `json:"state"`
	Reachable    bool     `json:"reachable"`
	RuntimeID    string   `json:"runtimeId"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type TerminalWait struct {
	Handle        string          `json:"handle"`
	Condition     string          `json:"condition"`
	Satisfied     bool            `json:"satisfied"`
	Status        string          `json:"status"`
	ExitCode      *int            `json:"exitCode"`
	BlockedReason json.RawMessage `json:"blockedReason,omitempty"`
}

type Screen struct {
	Handle    string   `json:"handle"`
	Status    string   `json:"status"`
	Source    string   `json:"source"`
	Truncated bool     `json:"truncated"`
	Limited   *bool    `json:"limited"`
	Tail      []string `json:"tail"`
}

type Prompt struct {
	RequestID          string   `json:"requestId"`
	Stages             []string `json:"stages"`
	Provider           string   `json:"provider"`
	Observation        string   `json:"observation"`
	ProcessIncarnation string   `json:"processIncarnation"`
}

type Send struct {
	Handle        string  `json:"handle"`
	Accepted      bool    `json:"accepted"`
	BytesWritten  int     `json:"bytesWritten"`
	RefusedReason string  `json:"refusedReason,omitempty"`
	Prompt        *Prompt `json:"prompt,omitempty"`
}

type Receipt struct {
	Terminal           string    `json:"terminal"`
	RequestID          string    `json:"requestId"`
	Stages             []string  `json:"stages"`
	Provider           string    `json:"provider"`
	Observation        string    `json:"observation"`
	ProcessIncarnation string    `json:"processIncarnation"`
	Submitted          bool      `json:"submitted"`
	At                 time.Time `json:"at"`
}

type Remote struct {
	client      Client
	Environment string
	RuntimeID   string
}

type promptDeliveryError struct {
	terminal string
	cause    error
}

type envelope[T any] struct {
	OK     bool `json:"ok"`
	Result T    `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Meta struct {
		RuntimeID *string `json:"runtimeId"`
	} `json:"_meta"`
}

func (e *promptDeliveryError) Error() string {
	return fmt.Sprintf("prompt delivery to terminal %s is unverified; inspect the task with status and read without resending", e.terminal)
}

func (e *promptDeliveryError) Unwrap() error { return e.cause }

func (c Client) AddEnvironment(ctx context.Context, name, pairing string) (Environment, error) {
	label := "orca environment add --name " + name
	out, runErr := c.runner.Run(ctx, "environment", "add", "--name", name, "--pairing-code", pairing, "--json")
	result, err := decode[struct {
		Environment Environment `json:"environment"`
	}](out, label, LocalRuntime)
	if err != nil {
		// The runner's error repeats the arguments, and with them the pairing code.
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return Environment{}, fmt.Errorf("%w (exit %d)", err, exit.ExitCode())
		}
		return Environment{}, err
	}
	if result.Environment.ID == "" || result.Environment.Name != name {
		return Environment{}, fmt.Errorf("%s saved %+v, not an environment named %s", label, result.Environment, name)
	}
	return result.Environment, nil
}

func (c Client) On(environment, runtimeID string) Remote {
	return Remote{client: c, Environment: environment, RuntimeID: runtimeID}
}

func (r Remote) Status(ctx context.Context) (RuntimeStatus, error) {
	result, err := scoped[struct {
		Runtime RuntimeStatus `json:"runtime"`
	}](ctx, r, "orca status", "status")
	if err != nil {
		return RuntimeStatus{}, err
	}
	if !result.Runtime.Reachable || result.Runtime.RuntimeID != r.RuntimeID {
		return result.Runtime, fmt.Errorf("environment %s reports runtime %q in state %s (reachable %t), not runtime %s", r.Environment, result.Runtime.RuntimeID, result.Runtime.State, result.Runtime.Reachable, r.RuntimeID)
	}
	return result.Runtime, nil
}

func (r Remote) AddRepo(ctx context.Context, path string) (Repo, error) {
	result, err := scoped[struct {
		Repo Repo `json:"repo"`
	}](ctx, r, "orca repo add", "repo", "add", "--path", path)
	if err != nil {
		return Repo{}, err
	}
	if result.Repo.ID == "" || result.Repo.Path != path {
		return Repo{}, fmt.Errorf("orca repo add on %s registered %+v, not the checkout at %s", r.Environment, result.Repo, path)
	}
	return result.Repo, nil
}

func (r Remote) Worktree(ctx context.Context, repoID, path string) (Worktree, error) {
	result, err := scoped[struct {
		Worktrees []Worktree `json:"worktrees"`
	}](ctx, r, "orca worktree list", "worktree", "list", "--repo", "id:"+repoID)
	if err != nil {
		return Worktree{}, err
	}
	for _, worktree := range result.Worktrees {
		if worktree.RepoID == repoID && worktree.Path == path {
			return worktree, nil
		}
	}
	return Worktree{}, fmt.Errorf("environment %s lists no worktree of repo %s at %s", r.Environment, repoID, path)
}

func (r Remote) CreateTerminal(ctx context.Context, worktreeID, title, command string) (Terminal, error) {
	result, err := scoped[struct {
		Terminal Terminal `json:"terminal"`
	}](ctx, r, "orca terminal create", "terminal", "create", "--worktree", "id:"+worktreeID, "--title", title, "--command", command)
	if err != nil {
		return Terminal{}, err
	}
	if result.Terminal.Handle == "" {
		return Terminal{}, fmt.Errorf("orca terminal create on %s returned no terminal handle", r.Environment)
	}
	return result.Terminal, nil
}

func (r Remote) Screen(ctx context.Context, handle string) (_ Screen, err error) {
	started := time.Now()
	defer func() { observe(ctx, "remote.screen", started, err) }()
	result, err := scoped[struct {
		Terminal Screen `json:"terminal"`
	}](ctx, r, "orca terminal read", "terminal", "read", "--terminal", handle, "--screen")
	if err != nil {
		return Screen{}, err
	}
	if err := r.answeredFor("orca terminal read", handle, result.Terminal.Handle); err != nil {
		return Screen{}, err
	}
	if result.Terminal.Source == screenUnavailable {
		return Screen{}, fmt.Errorf("terminal %s on %s has no rendered screen", handle, r.Environment)
	}
	return result.Terminal, nil
}

func (r Remote) WaitIdle(ctx context.Context, handle string, timeout time.Duration) (_ TerminalWait, err error) {
	started := time.Now()
	defer func() { observe(ctx, "remote.waitIdle", started, err) }()
	result, err := scoped[struct {
		Wait TerminalWait `json:"wait"`
	}](ctx, r, "orca terminal wait", "terminal", "wait", "--terminal", handle, "--for", tuiIdle, "--timeout-ms", strconv.FormatInt(timeout.Milliseconds(), 10))
	if err != nil {
		return TerminalWait{}, err
	}
	if err := r.answeredFor("orca terminal wait", handle, result.Wait.Handle); err != nil {
		return TerminalWait{}, err
	}
	if !result.Wait.Satisfied {
		return result.Wait, fmt.Errorf("terminal %s on %s was not tui-idle within %s: status %s, blocked %s", handle, r.Environment, timeout, result.Wait.Status, result.Wait.BlockedReason)
	}
	if result.Wait.Condition != tuiIdle {
		return TerminalWait{}, fmt.Errorf("terminal %s on %s satisfied %q, not %s", handle, r.Environment, result.Wait.Condition, tuiIdle)
	}
	return result.Wait, nil
}

func (r Remote) Key(ctx context.Context, handle, text string) error {
	started := time.Now()
	err := r.input(ctx, handle, "--text", text)
	observe(ctx, "remote.key", started, err)
	return err
}

func (r Remote) Enter(ctx context.Context, handle string) error {
	started := time.Now()
	err := r.input(ctx, handle, "--enter")
	observe(ctx, "remote.enter", started, err)
	return err
}

func (r Remote) input(ctx context.Context, handle string, args ...string) error {
	result, err := scoped[struct {
		Send Send `json:"send"`
	}](ctx, r, "orca terminal send", append([]string{"terminal", "send", "--terminal", handle}, args...)...)
	if err != nil {
		return err
	}
	if err := r.answeredFor("orca terminal send", handle, result.Send.Handle); err != nil {
		return err
	}
	if !result.Send.Accepted {
		return fmt.Errorf("terminal %s on %s refused input: %s", handle, r.Environment, result.Send.RefusedReason)
	}
	return nil
}

func (r Remote) answeredFor(label, handle, terminal string) error {
	if terminal == "" || terminal != handle {
		return fmt.Errorf("%s on %s answered for terminal %q, not %s", label, r.Environment, terminal, handle)
	}
	return nil
}

func (r Remote) Prompt(ctx context.Context, handle, text string, wait time.Duration) (Send, error) {
	args := []string{"terminal", "send", "--terminal", handle, "--text", text, "--enter", "--wait-submit", strconv.Itoa(int(wait.Seconds()))}
	result, err := scoped[struct {
		Send Send `json:"send"`
	}](ctx, r, "orca terminal send --enter", args...)
	if err != nil {
		return Send{}, &promptDeliveryError{terminal: handle, cause: err}
	}
	switch {
	case !result.Send.Accepted:
		return result.Send, fmt.Errorf("terminal %s on %s refused the prompt: %s", handle, r.Environment, result.Send.RefusedReason)
	case result.Send.Prompt == nil || result.Send.Prompt.RequestID == "":
		return result.Send, fmt.Errorf("terminal %s on %s accepted the prompt without a prompt receipt", handle, r.Environment)
	}
	return result.Send, nil
}

func (s Send) Submitted() bool {
	return s.Accepted && s.Prompt != nil && s.Prompt.Provider != "" && s.Prompt.Provider != oldHost && slices.Contains(s.Prompt.Stages, turnStarted)
}

func (s Send) Receipt(at time.Time) Receipt {
	return Receipt{
		Terminal:           s.Handle,
		RequestID:          s.Prompt.RequestID,
		Stages:             s.Prompt.Stages,
		Provider:           s.Prompt.Provider,
		Observation:        s.Prompt.Observation,
		ProcessIncarnation: s.Prompt.ProcessIncarnation,
		Submitted:          s.Submitted(),
		At:                 at,
	}
}

func scoped[T any](ctx context.Context, r Remote, label string, args ...string) (T, error) {
	out, runErr := r.client.runner.Run(ctx, append(args, "--environment", r.Environment, "--json")...)
	result, err := decode[T](out, label+" on "+r.Environment, r.RuntimeID)
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return result, errors.Join(runErr, err)
	}
	return result, err
}

func decode[T any](out []byte, label, runtimeID string) (T, error) {
	var answer envelope[T]
	if err := json.Unmarshal(out, &answer); err != nil {
		return answer.Result, fmt.Errorf("decode %s: %w", label, err)
	}
	answered := ""
	if answer.Meta.RuntimeID != nil {
		answered = *answer.Meta.RuntimeID
	}
	switch {
	case answer.Error != nil:
		return answer.Result, fmt.Errorf("%s: %s: %s", label, answer.Error.Code, answer.Error.Message)
	case !answer.OK:
		return answer.Result, fmt.Errorf("%s answered without ok: true", label)
	case answered != runtimeID:
		return answer.Result, fmt.Errorf("%s answered from runtime %q, not %s", label, answered, runtimeID)
	}
	return answer.Result, nil
}

func screenText(tail []string) string { return strings.Join(tail, "\n") }

func observe(ctx context.Context, operation string, started time.Time, err error, attrs ...any) {
	slog.InfoContext(ctx, "timing", append([]any{"operation", operation, "seconds", time.Since(started).Seconds(), "ok", err == nil}, attrs...)...)
}
