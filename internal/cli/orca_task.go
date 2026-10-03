package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
	Forward       orcaTunnel     `json:"forward"`
	Environment   string         `json:"environment"`
	EnvironmentID string         `json:"environmentId,omitempty"`
	RuntimeID     string         `json:"runtimeId,omitempty"`
	RepoID        string         `json:"repoId,omitempty"`
	WorktreeID    string         `json:"worktreeId,omitempty"`
	Agent         orca.Agent     `json:"agent"`
	Terminal      string         `json:"terminal,omitempty"`
	Bootstrap     []string       `json:"bootstrap,omitempty"`
	Receipts      []orca.Receipt `json:"receipts,omitempty"`
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
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("forward %s:%d to %s (see %s): %w", orca.Loopback, port, t.Host, t.Log, err)
	}
	return nil
}

func (t orcaTunnel) command(script string, stdin io.Reader) providers.Command {
	return providers.Command{Name: "ssh", Args: append(t.args(), "sh", "-c", remote.Quote(script)), Stdin: stdin}
}

func (t orcaTunnel) run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	return providers.Output(ctx, providers.OSRunner{}, t.command(script, stdin))
}

func (t orcaTunnel) dropKey(ctx context.Context, dir string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyDropTimeout)
	defer cancel()
	_, err := t.run(cleanup, orca.KeyDropScript(dir), nil)
	return err
}

func newOrcaTaskCmds(runner orca.Runner) []*cobra.Command {
	return []*cobra.Command{
		newOrcaCreateCmd(runner),
		newOrcaStatusCmd(runner),
		newOrcaReconnectCmd(runner),
		newOrcaSendCmd(runner),
		newOrcaReadCmd(runner),
	}
}

func newOrcaCreateCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	var ref, promptFile, title string
	var agent orca.Agent
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
			if err := agent.Validate(); err != nil {
				return err
			}
			prompt, err := readPrompt(cmd, promptFile)
			if err != nil {
				return err
			}
			session, err := flags.open()
			if err != nil {
				return err
			}
			runtime, err := orcaRuntime(session)
			if err != nil {
				return err
			}
			key, err := captureKey(cmd.Context(), session.Config.Orca.Keys[agent.KeyProvider()])
			if err != nil {
				return err
			}
			defer clear(key)
			control, err := state.NewOrcaControl()
			if err != nil {
				return err
			}
			result, err := session.Create(cmd.Context(), args[0], workspace.Source{Ref: cmp.Or(ref, session.Config.Ref)})
			if err != nil {
				return errors.Join(err, os.Remove(filepath.Dir(control)))
			}
			driver := orcaDriver{client: orca.NewClient(runner), state: session.Config.State(), log: session.Log}
			task, err := driver.task(result, agent, control)
			if err == nil {
				err = driver.launch(cmd.Context(), session, task, runtime, key, cmp.Or(title, result.Name), prompt)
			}
			if task == nil {
				return err
			}
			return errors.Join(err, emit(cmd.OutOrStdout(), task))
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag to check out (default config.ref)")
	cmd.Flags().StringVar(&agent.Kind, "agent", orca.AgentClaude, "worker CLI: claude or codex")
	cmd.Flags().StringVar(&agent.Model, "model", "", "the worker's model, passed through unchanged")
	cmd.Flags().StringVar(&agent.Effort, "effort", "", "the worker's effort, passed through unchanged")
	cmd.Flags().StringArrayVar(&agent.MCP, "mcp-config", nil, "MCP servers the worker may start: for claude an --mcp-config file or JSON, repeatable; for codex one TOML inline table for mcp_servers (default none)")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "file holding the worker's first prompt, or - for stdin")
	cmd.Flags().StringVar(&title, "title", "", "Orca terminal title (default the workspace name)")
	_ = cmd.MarkFlagRequired("model")
	_ = cmd.MarkFlagRequired("effort")
	_ = cmd.MarkFlagRequired("prompt-file")
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
			task, err := loadTask(cfg.State(), args[0])
			if err != nil {
				return err
			}
			health := orcaHealth{Task: task, Forward: task.Forward.up(cmd.Context())}
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
			task, err := loadTask(cfg.State(), args[0])
			if err != nil {
				return err
			}
			if err := task.Forward.ensure(cmd.Context(), task.Port); err != nil {
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
			task, err := loadTask(cfg.State(), args[0])
			if err != nil {
				return err
			}
			if !task.Forward.up(cmd.Context()) {
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
			task, err := loadTask(cfg.State(), args[0])
			if err != nil {
				return err
			}
			if !task.Forward.up(cmd.Context()) {
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

func (d orcaDriver) task(result *workspace.Result, agent orca.Agent, control string) (*orcaTask, error) {
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
		Forward:       orcaTunnel{Host: result.Name, Config: result.SSH.Config, Control: control, Log: d.state.OrcaForwardLog(result.Name)},
		Environment:   result.Name,
		Agent:         agent,
	}
	return task, d.save(task)
}

func (d orcaDriver) save(task *orcaTask) error {
	return state.Save(d.state.Orca(task.Workspace), task)
}

func (d orcaDriver) launch(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime, key []byte, title, prompt string) error {
	ready, pairing, err := d.start(ctx, session, task, runtime)
	if err != nil {
		return err
	}
	task.RuntimeID = ready.RuntimeID
	if err := d.save(task); err != nil {
		return err
	}
	if err := task.Forward.ensure(ctx, task.Port); err != nil {
		return err
	}
	environment, err := d.client.AddEnvironment(ctx, task.Environment, pairing)
	if err != nil {
		return err
	}
	task.EnvironmentID = environment.ID
	native := d.client.On(task.Environment, task.RuntimeID)
	if _, err := native.Status(ctx); err != nil {
		return err
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
	task.Bootstrap, err = native.Bootstrap(ctx, task.Terminal, orca.StartupOf(task.Agent), session.Config.Trusted(), orca.Poll{Interval: time.Second, Timeout: 3 * time.Minute})
	if err := errors.Join(err, d.save(task)); err != nil {
		return err
	}
	_, err = d.prompt(ctx, task, prompt)
	return err
}

func (d orcaDriver) start(ctx context.Context, session *workspace.Session, task *orcaTask, runtime orca.Runtime) (orca.Ready, string, error) {
	runtime.Port = task.Port
	result, err := session.Provider.Exec(ctx, task.Machine, []string{"sh", "-c", runtime.EnsureScript()}, nil)
	if err != nil {
		return orca.Ready{}, "", err
	}
	if result.ExitCode != 0 {
		return orca.Ready{}, "", fmt.Errorf("starting the Orca runtime on %s exited %d: %s", task.Machine, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	lines := strings.Split(strings.TrimSpace(string(result.Stdout)), "\n")
	ready, pairing, err := orca.ParseReady([]byte(lines[len(lines)-1]), task.Port)
	if err != nil {
		return orca.Ready{}, "", err
	}
	d.log.Info("the Orca runtime is ready", "machine", task.Machine, "runtime", ready.RuntimeID, "service", task.Service)
	return ready, pairing, nil
}

func (d orcaDriver) worker(ctx context.Context, native orca.Remote, task *orcaTask, title string, key []byte) (string, error) {
	out, err := task.Forward.run(ctx, orca.KeyDirScript, nil)
	if err != nil {
		return "", err
	}
	dir, err := orca.ParseKeyDir(out)
	if err != nil {
		return "", err
	}
	terminal, err := native.CreateTerminal(ctx, task.WorktreeID, title, task.Agent.Command(dir))
	if err != nil {
		dropped := task.Forward.dropKey(ctx, dir)
		return "", errors.Join(err, dropped)
	}
	payload := append(slices.Clone(key), '\n')
	defer clear(payload)
	write, cancel := context.WithTimeout(ctx, keyPipeTimeout)
	defer cancel()
	if _, err := task.Forward.run(write, orca.KeyWriteScript(dir), bytes.NewReader(payload)); err != nil {
		dropped := task.Forward.dropKey(ctx, dir)
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
