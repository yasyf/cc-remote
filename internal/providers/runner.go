package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Command struct {
	Name  string
	Args  []string
	Stdin io.Reader
}

func (c Command) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

type Runner interface {
	Run(ctx context.Context, cmd Command) (Result, error)
}

// A killed CLI's children keep its output pipes open; without WaitDelay, Wait blocks on them.
const waitDelay = 2 * time.Second

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, command Command) (Result, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.WaitDelay = waitDelay
	cmd.Stdin = command.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return result, fmt.Errorf("%s: %w", command.Name, ctx.Err())
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
		return result, nil
	case err != nil:
		return result, fmt.Errorf("%s: %w", command.Name, err)
	}
	return result, nil
}

func Output(ctx context.Context, runner Runner, cmd Command) ([]byte, error) {
	result, err := runner.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, &CommandError{Command: cmd.String(), Result: result}
	}
	return result.Stdout, nil
}

type CommandError struct {
	Command string
	Result  Result
}

func (e *CommandError) Error() string {
	detail := strings.TrimSpace(string(e.Result.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(e.Result.Stdout))
	}
	return fmt.Sprintf("%s: exit %d: %s", e.Command, e.Result.ExitCode, detail)
}

func (e *CommandError) Reports(grammar *regexp.Regexp) bool {
	for line := range strings.Lines(string(e.Result.Stderr) + "\n" + string(e.Result.Stdout)) {
		if grammar.MatchString(line) {
			return true
		}
	}
	return false
}

func Reports(err error, grammar *regexp.Regexp) bool {
	var command *CommandError
	return errors.As(err, &command) && command.Reports(grammar)
}

const Boundary = `[^A-Za-z0-9_.-]`

func Named(resource string) string {
	return `"?` + regexp.QuoteMeta(resource) + `"?`
}
