package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/remote"
)

const stepStop = 30 * time.Second

var stepRedial = 5 * time.Second

func newStepCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "step <machine> -- <command> [args...]",
		Short: "Run a command detached on a machine, replaying its output and exit status across dropped connections",
		Long:  "step saves the command's combined output and exit status under ~/.cc-remote/steps on the machine, reconnects after a drop, and exits with the command's own status. An interrupt stops the command.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			provider, err := openProvider(cfg, flags.provider)
			if err != nil {
				return err
			}
			var stdin []byte
			if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
				if stdin, err = io.ReadAll(os.Stdin); err != nil {
					return err
				}
			}
			return runStep(cmd.Context(), provider, args[0], args[1:], stdin, cmd.OutOrStdout())
		},
	}
	flags.bind(cmd)
	return cmd
}

func runStep(ctx context.Context, provider providers.Provider, machine string, argv []string, stdin []byte, out io.Writer) error {
	id := strings.ToLower(rand.Text()[:8])
	launch, finish := stepSupervisor(provider.Traits().Supervisor)
	script, run := stepScript(launch), stepRun(argv, finish)
	replayed := 0
	for {
		result, err := provider.Exec(ctx, machine, []string{"sh", "-c", script, "step", id, strconv.Itoa(replayed), run}, bytes.NewReader(stdin))
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), stopStep(ctx, provider, machine, id))
		}
		if err != nil {
			return err
		}
		if header, body, framed := bytes.Cut(result.Stdout, []byte("\n")); framed {
			var size, code int
			if _, err := fmt.Sscanf(string(header), "%d %d", &size, &code); err != nil {
				return fmt.Errorf("step %s on %s: %s: %s", id, machine, header, bytes.TrimSpace(result.Stderr))
			}
			body = body[:min(len(body), size)]
			if _, err := out.Write(body); err != nil {
				return err
			}
			replayed += len(body)
			if len(body) == size && code != 0 {
				return &CommandExit{code}
			}
			if len(body) == size {
				return nil
			}
		}
		slog.Warn("step connection dropped, reconnecting", "step", id, "machine", machine, "replayed", replayed, "exit", result.ExitCode, "stderr", string(bytes.TrimSpace(result.Stderr)))
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), stopStep(ctx, provider, machine, id))
		case <-time.After(stepRedial):
		}
	}
}

func stopStep(ctx context.Context, provider providers.Provider, machine, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepStop)
	defer cancel()
	_, err := provider.Exec(ctx, machine, []string{"sh", "-c", `dir="` + remote.StateDir + `/steps/$1"; test -e "$dir/exit" || kill -s TERM -- "-$(cat "$dir/pid")"`, "stop", id}, nil)
	return err
}

func stepSupervisor(supervisor providers.Supervisor) (launch, finish string) {
	switch supervisor {
	case providers.SupervisorSpriteEnv:
		return `sprite-env services create "cc-remote-step-$1" --cmd "$dir/run" --dir "$PWD" --duration 1ms --no-stream >&2`,
			`exec sprite-env services delete "cc-remote-step-${dir##*/}" > /dev/null 9>&-`
	case providers.SupervisorSetsid:
		return `nohup setsid "$dir/run" > /dev/null 2>&1 < /dev/null &`, ""
	}
	panic("unknown step supervisor " + string(supervisor))
}

func stepScript(launch string) string {
	return remote.Script(
		`dir="`+remote.StateDir+`/steps/$1" offset=$2`,
		`trap 'code=$?; test -n "${framed-}" || echo "failed $code"' EXIT`,
		`mkdir -p "${dir%/*}"`,
		`if mkdir -m 700 "$dir" 2> /dev/null; then`,
		`  cat > "$dir/stdin"`,
		`  export -p > "$dir/env"`,
		`  printf '%s\n' "$3" > "$dir/run"`,
		`  chmod 700 "$dir/run"`,
		"  "+launch,
		"fi",
		`until test -e "$dir/pid"; do sleep 0.1; done`,
		`flock "$dir/lock" true`,
		`test -e "$dir/exit" || { echo "`+remote.Prefix+`: step $1 ended without an exit status" >&2; exit 1; }`,
		`size=$(wc -c < "$dir/output")`,
		"framed=1",
		`echo "$((size - offset)) $(cat "$dir/exit")"`,
		`tail -c "+$((offset + 1))" "$dir/output" | head -c "$((size - offset))"`,
	)
}

func stepRun(argv []string, finish string) string {
	return strings.Join([]string{
		"#!/bin/sh",
		`dir=${0%/*}`,
		`exec 9> "$dir/lock"`,
		"flock 9",
		`if ! test -e "$dir/pid"; then`,
		`  echo $$ > "$dir/pid"`,
		`  (. "$dir/env" && exec ` + remote.QuoteAll(argv) + `) < "$dir/stdin" > "$dir/output" 2>&1 9>&-`,
		`  echo $? > "$dir/exit"`,
		"fi",
		finish,
	}, "\n")
}
