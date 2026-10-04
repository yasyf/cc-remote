package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/remote"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	orcaTaskSchema = 1
	keyTimeout     = 30 * time.Second
	keyPipeTimeout = 2 * time.Minute
	keyDropTimeout = 5 * time.Second
	submitWait     = 15 * time.Second
	gatewayStart   = 30 * time.Second
	gatewayPoll    = 100 * time.Millisecond
)

var ErrNotSubmitted = errors.New("the prompt was accepted, but no turn_started was observed")

type orcaTask struct {
	SchemaVersion int            `json:"schemaVersion"`
	Workspace     string         `json:"workspace"`
	Provider      string         `json:"provider"`
	Profile       string         `json:"profile"`
	Machine       string         `json:"machine"`
	ProjectRoot   string         `json:"projectRoot"`
	Service       string         `json:"service"`
	Port          int            `json:"port"`
	Forward       *orcaTunnel    `json:"forward,omitempty"`
	Gateway       *orcaGateway   `json:"gateway,omitempty"`
	Environment   string         `json:"environment"`
	EnvironmentID string         `json:"environmentId,omitempty"`
	RuntimeID     string         `json:"runtimeId,omitempty"`
	RepoID        string         `json:"repoId,omitempty"`
	WorktreeID    string         `json:"worktreeId,omitempty"`
	Agent         orca.Agent     `json:"agent"`
	Terminal      string         `json:"terminal,omitempty"`
	Bootstrap     []string       `json:"bootstrap,omitempty"`
	Receipts      []orca.Receipt `json:"receipts,omitempty"`
	Prepared      bool           `json:"prepared,omitempty"`
	Brief         *orcaArtifact  `json:"brief,omitempty"`
	BaseCommit    string         `json:"baseCommit,omitempty"`

	provider providers.Provider
}

type orcaGateway struct {
	Instance      string `json:"instance"`
	ContainerPort int    `json:"containerPort"`
	Lock          string `json:"lock"`
	Log           string `json:"log"`
	PID           int    `json:"pid,omitempty"`
}

type orcaLaunch struct {
	flags      selection
	ref, title string
	agent      orca.Agent
	existing   bool
}

type orcaTunnel struct {
	Host    string `json:"host"`
	Config  string `json:"config"`
	Control string `json:"control"`
	Log     string `json:"log"`
}

type orcaHealth struct {
	Task    *orcaTask           `json:"task"`
	Forward bool                `json:"forward"`
	Lease   json.RawMessage     `json:"lease,omitempty"`
	Runtime *orca.RuntimeStatus `json:"runtime,omitempty"`
	Error   string              `json:"error,omitempty"`
}

type orcaScreen struct {
	Terminal string   `json:"terminal"`
	Status   string   `json:"status"`
	Tail     []string `json:"tail"`
}

func (t orcaTunnel) args(extra ...string) []string {
	return slices.Concat([]string{"-F", t.Config, "-S", t.Control, "-o", "BatchMode=yes"}, extra, []string{t.Host})
}

func (t orcaTunnel) openArgs(port int) []string {
	forward := fmt.Sprintf("%s:%d:%s:%d", orca.Loopback, port, orca.Loopback, port)
	return t.args(
		"-o", "ControlMaster=yes", "-o", "ControlPersist=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-f", "-N", "-L", forward,
	)
}

func (t orcaTunnel) up(ctx context.Context) bool {
	return exec.CommandContext(ctx, "ssh", t.args("-O", "check")...).Run() == nil
}

func (t orcaTunnel) ensure(ctx context.Context, port int) error {
	if err := state.EnsureOrcaControl(t.Control); err != nil {
		return err
	}
	if t.up(ctx) {
		return nil
	}
	log, err := os.OpenFile(t.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	// The backgrounded master inherits these descriptors; pipes would hold Run open until it exits.
	cmd := exec.CommandContext(ctx, "ssh", t.openArgs(port)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("forward %s:%d to %s (see %s): %w", orca.Loopback, port, t.Host, t.Log, err)
	}
	return nil
}

func (t orcaTunnel) command(script string, stdin io.Reader) providers.Command {
	return providers.Command{Name: "ssh", Args: append(t.args(), "sh", "-c", remote.Quote(script)), Stdin: stdin}
}

func (g orcaGateway) up() bool {
	unlock, free, err := state.TryLock(g.Lock)
	if err != nil {
		return false
	}
	if free {
		unlock()
	}
	return !free
}

func (g *orcaGateway) ensure(ctx context.Context, workspace, config string, port int) error {
	if g.up() {
		return nil
	}
	cmd, err := forwardCommand(workspace, config)
	if err != nil {
		return err
	}
	log, err := os.OpenFile(g.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the gateway forward for %s: %w", workspace, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(gatewayStart)
	for !g.up() {
		select {
		case err := <-exited:
			return fmt.Errorf("the gateway forward for %s on %s:%d exited before it held the listener: %w: %s (see %s)", workspace, orca.Loopback, port, err, lastLine(g.Log), g.Log)
		case <-deadline:
			return fmt.Errorf("the gateway forward for %s did not hold %s:%d within %s (see %s); it is left running", workspace, orca.Loopback, port, gatewayStart, g.Log)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gatewayPoll):
		}
	}
	g.PID = cmd.Process.Pid
	return nil
}

var forwardCommand = func(workspace, config string) (*exec.Cmd, error) {
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(helper, "orca", "forward", workspace, "--config", config), nil
}

func (t *orcaTask) up(ctx context.Context) bool {
	if t.Gateway != nil {
		return t.Gateway.up()
	}
	return t.Forward.up(ctx)
}

func (t *orcaTask) ensure(ctx context.Context, config string) error {
	if t.Gateway != nil {
		return t.Gateway.ensure(ctx, t.Workspace, config, t.Port)
	}
	return t.Forward.ensure(ctx, t.Port)
}

func (t *orcaTask) shell(ctx context.Context, script string, stdin io.Reader) (providers.Result, error) {
	if t.Gateway != nil {
		return t.provider.Exec(ctx, t.Machine, []string{"sh", "-c", script}, stdin)
	}
	return providers.OSRunner{}.Run(ctx, t.Forward.command(script, stdin))
}

func (t *orcaTask) run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	if t.Gateway == nil {
		return providers.Output(ctx, providers.OSRunner{}, t.Forward.command(script, stdin))
	}
	result, err := t.provider.Exec(ctx, t.Machine, []string{"sh", "-c", script}, stdin)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, &providers.CommandError{Command: "sh -c on " + t.Machine, Result: result}
	}
	return result.Stdout, nil
}

func (t *orcaTask) dropKey(ctx context.Context, dir string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyDropTimeout)
	defer cancel()
	_, err := t.run(cleanup, orca.KeyDropScript(dir), nil)
	return err
}

func (l *orcaLaunch) bind(cmd *cobra.Command, mcp string) {
	l.flags.bind(cmd)
	cmd.Flags().StringVar(&l.ref, "ref", "", "branch or tag to check out (default config.ref)")
	cmd.Flags().StringVar(&l.agent.Kind, "agent", orca.AgentClaude, "worker CLI: claude or codex")
	cmd.Flags().StringVar(&l.agent.Model, "model", "", "the worker's model, passed through unchanged")
	cmd.Flags().StringVar(&l.agent.Effort, "effort", "", "the worker's effort, passed through unchanged")
	cmd.Flags().StringVar(&l.agent.Tier, "service-tier", "", "codex only: the worker's explicit service_tier, such as fast (default none)")
	cmd.Flags().StringArrayVar(&l.agent.MCP, "mcp-config", nil, mcp)
	cmd.Flags().StringVar(&l.title, "title", "", "Orca terminal title (default the workspace name)")
	_ = cmd.MarkFlagRequired("model")
	_ = cmd.MarkFlagRequired("effort")
}

func (l *orcaLaunch) run(cmd *cobra.Command, runner orca.Runner, name string, finish func(context.Context, orcaDriver, *orcaTask) error) error {
	session, err := l.flags.open()
	if err != nil {
		return err
	}
	runtime, err := orcaRuntime(session)
	if err != nil {
		return err
	}
	return l.start(cmd.Context(), cmd.OutOrStdout(), session, runner, runtime, name, finish)
}

func (l *orcaLaunch) start(ctx context.Context, out io.Writer, session *workspace.Session, runner orca.Runner, runtime orca.Runtime, name string, finish func(context.Context, orcaDriver, *orcaTask) error) error {
	unlock, err := claimTask(session.Config.State(), name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := absentTask(session.Config.State(), name); err != nil {
		return err
	}
	driver := orcaDriver{client: orca.NewClient(runner), state: session.Config.State(), log: session.Log}
	var task *orcaTask
	session.Retain = func(ctx context.Context, record *workspace.Record) (err error) {
		if l.existing {
			if err := owned(ctx, session.Provider, record, name); err != nil {
				return err
			}
		}
		task, err = driver.retain(ctx, session, record, l.agent, false)
		return err
	}
	var result *workspace.Result
	var key []byte
	defer func() { clear(key) }()
	if l.existing {
		if result, err = session.Resume(ctx, name); err != nil {
			return err
		}
		if err = checkRetained(ctx, session, result); err == nil {
			key, err = captureKey(ctx, session.Config.Orca.Keys[l.agent.KeyProvider()])
		}
	} else {
		if key, err = captureKey(ctx, session.Config.Orca.Keys[l.agent.KeyProvider()]); err != nil {
			return err
		}
		if result, err = session.Create(ctx, name, workspace.Source{Ref: cmp.Or(l.ref, session.Config.Ref)}); err != nil {
			return err
		}
	}
	if err == nil && task == nil {
		task, err = driver.task(result, l.agent, session.Provider)
	}
	if err == nil {
		err = driver.launch(ctx, session, task, runtime, key, cmp.Or(l.title, result.Name))
	}
	if err == nil {
		err = finish(ctx, driver, task)
	}
	if task == nil {
		return err
	}
	return errors.Join(err, emit(out, task))
}

func claimTask(dir state.Dir, name string) (func(), error) {
	if err := state.ValidateName(name); err != nil {
		return nil, err
	}
	unlock, held, err := state.TryLock(dir.Orca(name) + ".lock")
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, fmt.Errorf("another cc-remote command holds the Orca task for %s, so this one started and changed nothing", name)
	}
	return unlock, nil
}

func absentTask(dir state.Dir, name string) error {
	found, err := state.Load(dir.Orca(name), &orcaTask{})
	switch {
	case err != nil:
		return err
	case found:
		return fmt.Errorf("%s already has an Orca task under %s; a first launch neither resumes nor replaces it", name, dir)
	}
	return nil
}

func owned(ctx context.Context, provider providers.Provider, record *workspace.Record, name string) error {
	if record.Name != name || record.Machine == "" {
		return fmt.Errorf("the workspace record for %s names no machine of its own, so no worker starts on it", name)
	}
	if err := record.Source.Validate(); err != nil {
		return fmt.Errorf("the workspace record for %s holds no valid source: %w", name, err)
	}
	machine, err := provider.Get(ctx, record.Machine)
	if err != nil {
		return fmt.Errorf("find machine %s of workspace %s: %w", record.Machine, name, err)
	}
	if machine.ID != record.Machine || machine.Provider != record.Provider || machine.Labels[workspace.LabelWorkspace] != name {
		return fmt.Errorf("%s machine %s does not carry the ownership of workspace %s, so it is neither resumed nor given a worker", record.Provider, record.Machine, name)
	}
	return nil
}

func newOrcaTaskCmds(runner orca.Runner) []*cobra.Command {
	return []*cobra.Command{
		newOrcaCreateCmd(runner),
		newOrcaPrepareCmd(runner),
		newOrcaStatusCmd(runner),
		newOrcaReconnectCmd(runner),
		newOrcaSendCmd(runner),
		newOrcaReadCmd(runner),
		newOrcaCollectCmd(runner),
		newOrcaForwardCmd(),
	}
}

func newOrcaCreateCmd(runner orca.Runner) *cobra.Command {
	var launch orcaLaunch
	var promptFile string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a workspace, pair its Orca runtime with this Orca CLI, and start a worker on its checkout",
		Long: `create makes a fresh provider machine as the create command does, starts the Orca runtime there as a
provider service, forwards its loopback port over SSH, pairs it with this machine's Orca CLI as an
environment named after the workspace, adds the workspace's checkout as an Orca repo, and starts the
worker in an Orca terminal on that checkout. The worker's API key comes from the orca.keys command for
its provider and reaches only the worker, through a one-use pipe over SSH. stdout carries the task
record, also kept under the state directory for status, reconnect, send, and read.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := launch.agent.Validate(); err != nil {
				return err
			}
			prompt, err := readPrompt(cmd, promptFile)
			if err != nil {
				return err
			}
			return launch.run(cmd, runner, args[0], func(ctx context.Context, driver orcaDriver, task *orcaTask) error {
				_, err := driver.prompt(ctx, task, prompt)
				return err
			})
		},
	}
	launch.bind(cmd, "MCP servers the worker may start: for claude an --mcp-config file or JSON, repeatable; for codex one TOML inline table for mcp_servers (default none)")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "file holding the worker's first prompt, or - for stdin")
	_ = cmd.MarkFlagRequired("prompt-file")
	return cmd
}

func newOrcaPrepareCmd(runner orca.Runner) *cobra.Command {
	var launch orcaLaunch
	var briefFile string
	cmd := &cobra.Command{
		Use:   "prepare <name>",
		Short: "Create a workspace and start an idle worker on its checkout with its brief on the machine, sending no prompt",
		Long: `prepare runs create's machine, runtime, pairing, checkout, key and worker steps once, stopping when the
worker is idle. It copies the brief file byte for byte to a private task directory on the machine, outside
the checkout, verifies its SHA-256 and length, and records the checkout's HEAD as baseCommit. It sends the
worker no input; Orca's worker-start delivers the first task. stdout carries the task record, prepared: true.

With --existing, prepare starts the first worker of a recorded workspace instead of creating one: it resumes
the exact recorded machine and keeps its checkout as it is. It refuses a recorded Orca task, a machine without
the workspace's ownership, a checkout or origin that differs from the config, and earlier Orca runtime state,
and it takes no --ref.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := launch.agent.Validate(); err != nil {
				return err
			}
			if err := launch.agent.InlineMCP(); err != nil {
				return err
			}
			if launch.existing && cmd.Flags().Changed("ref") {
				return errors.New("--existing starts the first worker on the workspace's retained checkout, so it takes no --ref")
			}
			brief, err := readBrief(briefFile)
			if err != nil {
				return err
			}
			return launch.run(cmd, runner, args[0], func(ctx context.Context, driver orcaDriver, task *orcaTask) error {
				return driver.publish(ctx, task, brief)
			})
		},
	}
	launch.bind(cmd, "MCP servers the worker may start, inline only: for claude one JSON object per flag; for codex one TOML inline table for mcp_servers (default none)")
	cmd.Flags().StringVar(&briefFile, "brief-file", "", "file holding the worker's complete brief, copied unchanged to the machine")
	cmd.Flags().BoolVar(&launch.existing, "existing", false, "start the first worker on the recorded workspace's machine and retained checkout instead of creating one")
	_ = cmd.MarkFlagRequired("brief-file")
	return cmd
}

func newOrcaCollectCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	var request orcaCollect
	cmd := &cobra.Command{
		Use:   "collect <name>",
		Short: "Copy a prepared task's report and patch from its task directory into a new local directory",
		Long: `collect reads two regular files, never links, from the directory holding the prepared task's brief. The
report is JSON: schemaVersion 1, the prepared baseCommit, the patch's sha256 and bytes, and files naming each
changed path with its sha256 or deleted: true. On the recorded live runtime and an unmoved HEAD, collect checks
the patch against the report, writes both files unchanged into the new --output directory, and prints their
hashes and bounded git status entries. The machine is unchanged; a matching report does not prove completeness.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			collection, err := request.collect(cmd.Context(), orca.NewClient(runner), task)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), collection)
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&request.report, "report-file", "", "absolute path of the report in the task's directory on the machine")
	cmd.Flags().StringVar(&request.patch, "patch-file", "", "absolute path of the patch in the task's directory on the machine")
	cmd.Flags().StringVar(&request.output, "output", "", "local directory to create for the two files; it must not exist yet")
	_ = cmd.MarkFlagRequired("report-file")
	_ = cmd.MarkFlagRequired("patch-file")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func newOrcaStatusCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Report a task's forward and its Orca runtime as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			health := orcaHealth{Task: task, Forward: task.up(cmd.Context())}
			if task.Gateway != nil {
				if health.Lease, err = readLease(cfg.State(), task.Workspace, task.Gateway.Instance); err != nil {
					return err
				}
			}
			err = errors.New("the forward is down; run cc-remote orca reconnect " + task.Workspace)
			if health.Forward {
				var status orca.RuntimeStatus
				status, err = orca.NewClient(runner).On(task.Environment, task.RuntimeID).Status(cmd.Context())
				health.Runtime = &status
			}
			if err != nil {
				health.Error = err.Error()
			}
			return errors.Join(emit(cmd.OutOrStdout(), health), err)
		},
	}
	flags.bind(cmd)
	return cmd
}

func newOrcaReconnectCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "reconnect <name>",
		Short: "Restore a task's SSH forward to its recorded live Orca runtime",
		Long: `reconnect reopens the recorded SSH forward when it is down and confirms the paired environment
answers from the recorded live runtime. It does not resume a machine or start a runtime or worker.
Recovery after a runtime exits or a machine loses its processes is not supported by this command.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			if err := task.ensure(cmd.Context(), cfg.Path); err != nil {
				return err
			}
			if err := (orcaDriver{state: cfg.State()}).save(task); err != nil {
				return err
			}
			if _, err := orca.NewClient(runner).On(task.Environment, task.RuntimeID).Status(cmd.Context()); err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), task)
		},
	}
	flags.bind(cmd)
	return cmd
}

func newOrcaSendCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	var promptFile string
	cmd := &cobra.Command{
		Use:   "send <name>",
		Short: "Send a prompt to a task's worker terminal and print its receipt",
		Long: `send submits one prompt through orca terminal send and records the receipt. When input was
accepted but no turn start was observed, inspect the task with status and read. The command keeps
the accepted receipt and never resends the prompt. Native request replay is not exposed because
the supported Orca runtime can deliver the prompt again when replaying an accepted request.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt, err := readPrompt(cmd, promptFile)
			if err != nil {
				return err
			}
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			if task.Prepared {
				return fmt.Errorf("%s waits for Orca's worker-start; reach its worker through the home Dispatch, not this terminal", task.Workspace)
			}
			if !task.up(cmd.Context()) {
				return errors.New("the forward is down; run cc-remote orca reconnect " + task.Workspace)
			}
			driver := orcaDriver{client: orca.NewClient(runner), state: cfg.State(), log: slog.Default()}
			receipt, err := driver.prompt(cmd.Context(), task, prompt)
			if receipt == nil {
				return err
			}
			return errors.Join(err, emit(cmd.OutOrStdout(), receipt))
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "file holding the prompt, or - for stdin")
	_ = cmd.MarkFlagRequired("prompt-file")
	return cmd
}

func newOrcaReadCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "read <name>",
		Short: "Print the rendered screen of a task's worker terminal as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			if !task.up(cmd.Context()) {
				return errors.New("the forward is down; run cc-remote orca reconnect " + task.Workspace)
			}
			screen, err := orca.NewClient(runner).On(task.Environment, task.RuntimeID).Screen(cmd.Context(), task.Terminal)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), orcaScreen{Terminal: screen.Handle, Status: screen.Status, Tail: orca.Redact(screen.Tail)})
		},
	}
	flags.bind(cmd)
	return cmd
}

type orcaDriver struct {
	client orca.Client
	state  state.Dir
	log    *slog.Logger
}

func (d orcaDriver) task(result *workspace.Result, agent orca.Agent, provider providers.Provider) (*orcaTask, error) {
	port, err := workspace.FreePort()
	if err != nil {
		return nil, err
	}
	task := &orcaTask{
		SchemaVersion: orcaTaskSchema,
		Workspace:     result.Name,
		Provider:      result.Provider,
		Profile:       result.Profile,
		Machine:       result.Machine,
		ProjectRoot:   result.ProjectRoot,
		Service:       orca.RuntimeService,
		Port:          port,
		Environment:   result.Name,
		Agent:         agent,
		provider:      provider,
	}
	switch {
	case result.Compute != nil:
		task.Gateway = &orcaGateway{Instance: result.Machine, ContainerPort: result.Compute.ContainerPort, Lock: d.state.OrcaGatewayLock(result.Name, result.Machine), Log: d.state.OrcaForwardLog(result.Name)}
	default:
		control, err := state.NewOrcaControl()
		if err != nil {
			return nil, err
		}
		task.Forward = &orcaTunnel{Host: result.Name, Config: result.SSH.Config, Control: control, Log: d.state.OrcaForwardLog(result.Name)}
	}
	return task, d.save(task)
}

func (d orcaDriver) save(task *orcaTask) error {
	return state.Save(d.state.Orca(task.Workspace), task)
}

func (d orcaDriver) observe(operation string, started time.Time, err error) {
	d.log.Info("timing", "operation", operation, "seconds", time.Since(started).Seconds(), "ok", err == nil)
}

func (d orcaDriver) retain(ctx context.Context, session *workspace.Session, record *workspace.Record, agent orca.Agent, resumed bool) (*orcaTask, error) {
	if record.Compute == nil {
		return nil, nil
	}
	var task *orcaTask
	var err error
	if resumed {
		if task, err = loadTask(d.state, record.Name); err != nil {
			return nil, err
		}
		if task.Gateway == nil || task.Gateway.Instance != record.Machine {
			return nil, fmt.Errorf("the saved Orca task for %s does not forward instance %s, so no keeper is started for it", record.Name, record.Machine)
		}
		task.provider = session.Provider
	} else {
		allocated := &workspace.Result{Name: record.Name, Provider: record.Provider, Profile: record.Profile, Machine: record.Machine, ProjectRoot: session.ProjectRoot(), Compute: record.Compute}
		if task, err = d.task(allocated, agent, session.Provider); err != nil {
			return nil, err
		}
	}
	return task, errors.Join(task.ensure(ctx, session.Config.Path), d.save(task))
}

func orcaRetain(session *workspace.Session, resumed bool) func(context.Context, *workspace.Record) error {
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	return func(ctx context.Context, record *workspace.Record) error {
		_, err := driver.retain(ctx, session, record, orca.Agent{}, resumed)
		return err
	}
}

func (d orcaDriver) launch(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime, key []byte, title string) error {
	ready, pairing, err := d.start(ctx, session, task, runtime)
	if err != nil {
		return err
	}
	task.RuntimeID = ready.RuntimeID
	if err := d.save(task); err != nil {
		return err
	}
	if err := task.ensure(ctx, session.Config.Path); err != nil {
		return err
	}
	environment, err := d.client.AddEnvironment(ctx, task.Environment, pairing)
	if err != nil {
		return err
	}
	task.EnvironmentID = environment.ID
	native := d.client.On(task.Environment, task.RuntimeID)
	if _, err := native.Status(ctx); err != nil {
		return errors.Join(err, d.save(task))
	}
	repo, err := native.AddRepo(ctx, task.ProjectRoot)
	if err != nil {
		return err
	}
	worktree, err := native.Worktree(ctx, repo.ID, task.ProjectRoot)
	if err != nil {
		return err
	}
	task.RepoID, task.WorktreeID = repo.ID, worktree.ID
	if err := d.save(task); err != nil {
		return err
	}
	if task.Terminal, err = d.worker(ctx, native, task, title, key); err != nil {
		return err
	}
	if err := d.save(task); err != nil {
		return err
	}
	d.log.Info("started the worker", "terminal", task.Terminal, "agent", task.Agent.Kind, "model", task.Agent.Model)
	started := time.Now()
	startup, err := orcaStartup(session, task)
	d.observe("startup.config", started, err)
	if err != nil {
		return err
	}
	task.Bootstrap, err = native.Bootstrap(ctx, task.Terminal, startup, session.Config.Trusted(), orca.Poll{Interval: time.Second, Timeout: 3 * time.Minute})
	return errors.Join(err, d.save(task))
}

func orcaServe(ctx context.Context, session *workspace.Session, result *workspace.Result, resumed bool) (string, error) {
	runtime, err := orcaRuntime(session)
	if err != nil {
		return "", err
	}
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	var task *orcaTask
	if resumed || result.Compute != nil {
		if task, err = loadTask(driver.state, result.Name); err != nil {
			return "", err
		}
		task.provider = session.Provider
	} else if task, err = driver.task(result, orca.Agent{}, session.Provider); err != nil {
		return "", err
	}
	return driver.serve(ctx, session, task, runtime, resumed)
}

func (d orcaDriver) serve(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime, resumed bool) (string, error) {
	if resumed {
		pairing, err := d.resumed(ctx, session, task, runtime)
		if err != nil {
			return "", err
		}
		return pairing, task.ensure(ctx, session.Config.Path)
	}
	ready, pairing, err := d.start(ctx, session, task, runtime)
	if err != nil {
		return "", err
	}
	task.RuntimeID = ready.RuntimeID
	if err := d.save(task); err != nil {
		return "", err
	}
	return pairing, task.ensure(ctx, session.Config.Path)
}

func (t *orcaTask) runtime(runtime orca.Runtime) orca.Runtime {
	runtime.Port = t.Port
	if t.Gateway != nil {
		runtime.Port, runtime.Advertise = t.Gateway.ContainerPort, fmt.Sprintf("ws://%s:%d", orca.Loopback, t.Port)
	}
	return runtime
}

func (d orcaDriver) start(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime) (orca.Ready, string, error) {
	runtime = task.runtime(runtime)
	result, err := session.Provider.Exec(ctx, task.Machine, []string{"sh", "-c", runtime.EnsureScript()}, nil)
	if err != nil {
		return orca.Ready{}, "", err
	}
	if result.ExitCode != 0 {
		return orca.Ready{}, "", fmt.Errorf("starting the Orca runtime on %s exited %d: %s", task.Machine, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	ready, pairing, err := orca.ParseReady(lastReady(result.Stdout), runtime.Port, task.Port)
	if err != nil {
		return orca.Ready{}, "", err
	}
	d.log.Info("the Orca runtime is ready", "machine", task.Machine, "runtime", ready.RuntimeID, "service", task.Service)
	return ready, pairing, nil
}

func (d orcaDriver) resumed(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime) (string, error) {
	runtime = task.runtime(runtime)
	result, err := session.Provider.Exec(ctx, task.Machine, []string{"sh", "-c", orca.ProbeScript}, nil)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("%s has no running Orca runtime %s to resume (%s); none is started in its place, and the saved task is kept", task.Workspace, task.RuntimeID, bytes.TrimSpace(result.Stderr))
	}
	ready, pairing, err := orca.ParseReady(lastReady(result.Stdout), runtime.Port, task.Port)
	if err != nil {
		return "", err
	}
	if ready.RuntimeID != task.RuntimeID {
		return "", fmt.Errorf("%s now answers as Orca runtime %s, not its saved %s; the other runtime is not adopted, and the saved task and every process are kept", task.Workspace, ready.RuntimeID, task.RuntimeID)
	}
	d.log.Info("the Orca runtime survived the resume", "machine", task.Machine, "runtime", ready.RuntimeID)
	return pairing, nil
}

func lastReady(stdout []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	return []byte(lines[len(lines)-1])
}

func (d orcaDriver) worker(ctx context.Context, native orca.Remote, task *orcaTask, title string, key []byte) (string, error) {
	out, err := task.run(ctx, orca.KeyDirScript, nil)
	if err != nil {
		return "", err
	}
	dir, err := orca.ParseKeyDir(out)
	if err != nil {
		return "", err
	}
	terminal, err := native.CreateTerminal(ctx, task.WorktreeID, title, task.Agent.Command(dir))
	if err != nil {
		dropped := task.dropKey(ctx, dir)
		return "", errors.Join(err, dropped)
	}
	payload := append(slices.Clone(key), '\n')
	defer clear(payload)
	write, cancel := context.WithTimeout(ctx, keyPipeTimeout)
	defer cancel()
	if _, err := task.run(write, orca.KeyWriteScript(dir), bytes.NewReader(payload)); err != nil {
		dropped := task.dropKey(ctx, dir)
		return terminal.Handle, errors.Join(fmt.Errorf("terminal %s never read its key: %w", terminal.Handle, err), dropped)
	}
	return terminal.Handle, nil
}

func (d orcaDriver) prompt(ctx context.Context, task *orcaTask, prompt string) (*orca.Receipt, error) {
	send, err := d.client.On(task.Environment, task.RuntimeID).Prompt(ctx, task.Terminal, prompt, submitWait)
	if err != nil {
		return nil, err
	}
	receipt := send.Receipt(time.Now().UTC())
	task.Receipts = append(task.Receipts, receipt)
	if err := d.save(task); err != nil {
		return &receipt, err
	}
	if !receipt.Submitted {
		return &receipt, fmt.Errorf("%w for request %s (stages %v, provider %s); inspect the task with status and read before deciding what to send next", ErrNotSubmitted, receipt.RequestID, receipt.Stages, receipt.Provider)
	}
	return &receipt, nil
}

func orcaRuntime(session *workspace.Session) (orca.Runtime, error) {
	cfg := session.Config
	inventory, err := images.Load(cfg.ScriptPath(cfg.Inventory))
	if err != nil {
		return orca.Runtime{}, err
	}
	dir, err := inventory.ToolDir(session.Profile, cfg.Orca.Tool)
	if err != nil {
		return orca.Runtime{}, fmt.Errorf("orca.tool: %w", err)
	}
	return orca.Runtime{
		Entry:      dir + "/" + cfg.Orca.Entry,
		Display:    cfg.Orca.Display,
		Args:       cfg.Orca.Args,
		Supervised: session.Provider.Traits().Supervisor == providers.SupervisorSpriteEnv,
	}, nil
}

func orcaStartup(session *workspace.Session, task *orcaTask) (orca.Startup, error) {
	if task.Agent.Kind != orca.AgentCodex {
		return orca.StartupOf(task.Agent), nil
	}
	cfg := session.Config
	inventory, err := images.Load(cfg.ScriptPath(cfg.Inventory))
	if err != nil {
		return orca.Startup{}, err
	}
	return orca.CodexStartup(orca.HookReview{
		Captain: captainPins(inventory),
		Exec: func(ctx context.Context, argv []string) ([]byte, error) {
			result, err := session.Provider.Exec(ctx, task.Machine, argv, nil)
			if err != nil {
				return nil, err
			}
			if result.ExitCode != 0 {
				return nil, fmt.Errorf("the Codex hook probe on %s exited %d", task.Machine, result.ExitCode)
			}
			return result.Stdout, nil
		},
	}), nil
}

func captainPins(inventory images.Inventory) []orca.Pin {
	var pins []orca.Pin
	for _, plugin := range inventory.Claude.Plugins {
		if strings.HasPrefix(plugin.ID, orca.CaptainPlugin+"@") {
			pins = append(pins, orca.Pin{ID: plugin.ID, Version: plugin.Version})
		}
	}
	return pins
}

func captureKey(ctx context.Context, command []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, keyTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
	if err != nil {
		clear(out)
		return nil, fmt.Errorf("key command %s: %w", strings.Join(command, " "), err)
	}
	key := bytes.TrimSpace(out)
	if len(key) == 0 || bytes.ContainsAny(key, "\r\n") {
		clear(out)
		return nil, fmt.Errorf("key command %s must print exactly one non-empty line", strings.Join(command, " "))
	}
	return key, nil
}

func readPrompt(cmd *cobra.Command, path string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the prompt: %w", err)
	}
	prompt := strings.TrimSpace(string(raw))
	if prompt == "" {
		return "", fmt.Errorf("the prompt in %s is empty", path)
	}
	return prompt, nil
}

func openTask(cfg *config.Config, name string) (*orcaTask, error) {
	task, err := loadTask(cfg.State(), name)
	if err != nil || task.Gateway == nil {
		return task, err
	}
	if task.provider, err = openProvider(cfg, task.Provider); err != nil {
		return nil, err
	}
	return task, nil
}

func readLease(dir state.Dir, name, instance string) (json.RawMessage, error) {
	raw, err := os.ReadFile(dir.OrcaLease(name, instance))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func loadTask(dir state.Dir, name string) (*orcaTask, error) {
	task := &orcaTask{}
	found, err := state.Load(dir.Orca(name), task)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no Orca task is recorded for %s under %s; cc-remote orca create makes one", name, dir)
	}
	if task.SchemaVersion != orcaTaskSchema {
		return nil, fmt.Errorf("%s holds task schema %d, not %d", dir.Orca(name), task.SchemaVersion, orcaTaskSchema)
	}
	return task, nil
}
