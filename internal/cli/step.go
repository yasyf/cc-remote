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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	stepStop    = 30 * time.Second
	stepTrailer = remote.Prefix + " step"
	stepMemory  = "/dev/shm/" + remote.Prefix
	stepHold    = `awake -X PUT -d '{"expire":"1h"}'`
	stepRelease = "awake -X DELETE"
)

var (
	stepRedial = 5 * time.Second
	stepID     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	stepAwake  = map[providers.Supervisor]string{
		providers.SupervisorSpriteEnv: `awake() { curl -fsS -m 5 --unix-socket /.sprite/api.sock -H 'Host: sprite' -H 'Content-Type: application/json' "http://sprite/v1/tasks/cc-remote-step-$step" "$@" > /dev/null; }`,
	}
	stepRenew = "1800"
	stepRetry = "30"
)

func newStepCmd() *cobra.Command {
	var flags selection
	var forwardStdin bool
	var id string
	cmd := &cobra.Command{
		Use:   "step <machine> -- <command> [args...]",
		Short: "Run a command detached on a machine, streaming its output and exit status across dropped connections",
		Long: `step runs the command detached on the machine, saves its combined output and exit status under
~/.cc-remote/steps/<id>, streams the output as the command writes it, reconnects after a drop from the
last byte it streamed, and exits with the command's own status. It prints the step's id to stderr
first. A step with the id of one that already started follows that command instead of starting
another, as step attach does. A Sprite stays awake while the command runs, with or without
a connection. An interrupt stops the command. The command sources the keys that
keys push wrote to the machine's memory, then reads an empty stdin unless --stdin forwards this one,
read to its end first and held in the machine's memory, never on its disk, until the command opens it.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				id = strings.ToLower(rand.Text()[:8])
			}
			if !stepID.MatchString(id) {
				return fmt.Errorf("--id %q must be 1 to 63 lowercase letters, digits and dashes, starting with a letter or digit", id)
			}
			provider, err := flags.machines()
			if err != nil {
				return err
			}
			var stdin []byte
			if forwardStdin {
				if stdin, err = io.ReadAll(os.Stdin); err != nil {
					return err
				}
			}
			return runStep(cmd.Context(), provider, args[0], id, 0, args[1:], stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&forwardStdin, "stdin", false, "forward this stdin to the command, read to its end first")
	cmd.Flags().StringVar(&id, "id", "", "the step's id, so a caller that loses this process can attach to the command (default random)")
	flags.bind(cmd)
	cmd.AddCommand(newStepAttachCmd())
	return cmd
}

func newStepAttachCmd() *cobra.Command {
	var flags selection
	var offset int
	cmd := &cobra.Command{
		Use:   "attach <machine> <id>",
		Short: "Follow a step that already started, streaming its output from a byte offset and exiting with its status",
		Long:  "attach follows the step with this id as step does once it has started, from --offset, the count of output bytes the caller already holds. An interrupt stops the command.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stepID.MatchString(args[1]) {
				return fmt.Errorf("step id %q is not one step prints", args[1])
			}
			if offset < 0 {
				return fmt.Errorf("--offset %d must be 0 or more", offset)
			}
			provider, err := flags.machines()
			if err != nil {
				return err
			}
			return runStep(cmd.Context(), provider, args[0], args[1], offset, nil, nil, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().IntVar(&offset, "offset", 0, "output bytes already read, which attach skips")
	flags.bind(cmd)
	return cmd
}

func (f *selection) machines() (providers.Provider, error) {
	cfg, err := f.load()
	if err != nil {
		return nil, err
	}
	return openProvider(cfg, f.provider)
}

func runStep(ctx context.Context, provider providers.Provider, machine, id string, offset int, argv []string, stdin []byte, out, notes io.Writer) error {
	if _, err := fmt.Fprintf(notes, "%s: step %s on %s\n", remote.Prefix, id, machine); err != nil {
		return err
	}
	supervisor := provider.Traits().Supervisor
	launch, finish := stepSupervisor(supervisor)
	run := ""
	if argv != nil {
		run = stepRun(argv, stepAwake[supervisor], finish)
	}
	script, streamed := stepScript(launch, stepAwake[supervisor]), offset
	for {
		var got byteCount
		result, err := stream(ctx, provider, machine, []string{"sh", "-c", script, "step", id, strconv.Itoa(streamed), run}, bytes.NewReader(stdin), io.MultiWriter(out, &got))
		streamed += int(got)
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), stopStep(ctx, provider, machine, id))
		}
		if err != nil {
			return err
		}
		size, code, ended, failed := stepEnd(result.Stderr)
		switch {
		case failed:
			return fmt.Errorf("step %s on %s: %s", id, machine, bytes.TrimSpace(result.Stderr))
		case ended && size == streamed && code != 0:
			return &CommandExit{code}
		case ended && size == streamed:
			return nil
		case ended && size < streamed:
			return fmt.Errorf("step %s on %s ended with %d bytes of output, fewer than the %d already read", id, machine, size, streamed)
		}
		slog.Warn("step connection dropped, reconnecting", "step", id, "machine", machine, "streamed", streamed, "exit", result.ExitCode, "stderr", string(bytes.TrimSpace(result.Stderr)))
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), stopStep(ctx, provider, machine, id))
		case <-time.After(stepRedial):
		}
	}
}

func stream(ctx context.Context, provider providers.Provider, machine string, cmd []string, stdin io.Reader, out io.Writer) (providers.Result, error) {
	if streamer, ok := provider.(providers.Streamer); ok {
		return streamer.Stream(ctx, machine, cmd, stdin, out)
	}
	result, err := provider.Exec(ctx, machine, cmd, stdin)
	if err != nil {
		return result, err
	}
	_, err = out.Write(result.Stdout)
	return result, err
}

type byteCount int

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

func stepEnd(stderr []byte) (size, code int, ended, failed bool) {
	for line := range strings.Lines(string(stderr)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), stepTrailer+" ")
		if !ok {
			continue
		}
		if _, err := fmt.Sscanf(rest, "failed %d", &code); err == nil {
			return 0, code, false, true
		}
		if _, err := fmt.Sscanf(rest, "ended %d %d", &size, &code); err == nil {
			return size, code, true, false
		}
	}
	return 0, 0, false, false
}

func stopStep(ctx context.Context, provider providers.Provider, machine, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepStop)
	defer cancel()
	script := `step=$1 dir="` + remote.StateDir + `/steps/$1"; test -e "$dir/exit" || kill -s TERM -- "-$(cat "$dir/pid")"`
	if awake := stepAwake[provider.Traits().Supervisor]; awake != "" {
		script = awake + "\n" + script + "\n" + stepRelease
	}
	_, err := provider.Exec(ctx, machine, []string{"sh", "-c", script, "stop", id}, nil)
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

func stepScript(launch, awake string) string {
	held := []string{}
	if awake != "" {
		held = []string{
			"  " + awake,
			"  tries=0",
			"  until " + stepHold + "; do",
			"    tries=$((tries + 1))",
			`    test "$tries" -lt 3 || { rm -rf "$dir"; echo "` + remote.Prefix + `: step $1 could not hold the machine awake, so its command never started" >&2; exit 1; }`,
			"    sleep 2",
			"  done",
		}
	}
	return remote.Script(slices.Concat(
		[]string{
			`step=$1 dir="` + remote.StateDir + `/steps/$1" memory="` + stepMemory + `/steps/$1" offset=$2`,
			`trap 'code=$?; test -n "${framed-}" || echo "` + stepTrailer + ` failed $code" >&2' EXIT`,
			`mkdir -p "${dir%/*}"`,
			`if test -n "$3" && mkdir -m 700 "$dir" 2> /dev/null; then`,
		},
		held,
		[]string{
			`  (umask 077 && mkdir -p "${memory%/*}" && cat > "$memory.part" && mv "$memory.part" "$memory")`,
			`  export -p > "$dir/env"`,
			`  printf '%s\n' "$3" > "$dir/run"`,
			`  chmod 700 "$dir/run"`,
			`  : > "$dir/output"`,
			"  " + launch,
			"fi",
			`test -d "$dir" || { echo "` + remote.Prefix + `: no step $1 started on this machine" >&2; exit 1; }`,
			`until test -e "$dir/pid"; do sleep 0.1; done`,
			`if test -e "$dir/exit"; then tail -c "+$((offset + 1))" "$dir/output"; else tail -c "+$((offset + 1))" -f --pid="$(cat "$dir/pid")" "$dir/output"; fi`,
			`flock "$dir/lock" true`,
			`test -e "$dir/exit" || { echo "` + remote.Prefix + `: step $1 stopped before its command recorded an exit status: it was stopped, or the machine suspended mid-step, which stops every service and the command with it; run the step again" >&2; exit 1; }`,
			"framed=1",
			`echo "` + stepTrailer + ` ended $(wc -c < "$dir/output") $(cat "$dir/exit")" >&2`,
		},
	)...)
}

func stepRun(argv []string, awake, finish string) string {
	lines := []string{
		"#!/bin/sh",
		`dir=${0%/*}`,
		`step=${dir##*/}`,
		`memory="` + stepMemory + `/steps/$step"`,
		`exec 9> "$dir/lock"`,
		"flock 9",
		`if ! test -e "$dir/pid"; then`,
		`  echo $$ > "$dir/pid"`,
	}
	if awake != "" {
		lines = append(lines,
			"  "+awake,
			`  trap 'kill "$renew" 2> /dev/null; `+stepRelease+`' EXIT`,
			`  trap 'exit 143' HUP INT TERM`,
			`  (while sleep `+stepRenew+`; do until `+stepHold+`; do sleep `+stepRetry+`; done; done) > /dev/null 2>&1 8<&- 9>&- &`,
			"  renew=$!",
		)
	}
	lines = append(lines,
		`  exec 8< "$memory"`,
		`  rm -f "$memory"`,
		`  (. "$dir/env" && { test ! -r `+orca.KeysFile+` || . `+orca.KeysFile+`; } && exec `+remote.QuoteAll(argv)+`) <&8 > "$dir/output" 2>&1 8<&- 9>&-`,
		`  echo $? > "$dir/exit"`,
	)
	if awake != "" {
		lines = append(lines, `  kill "$renew"`, "  "+stepRelease, "  trap - EXIT HUP INT TERM")
	}
	return strings.Join(append(lines, "fi", finish), "\n")
}
