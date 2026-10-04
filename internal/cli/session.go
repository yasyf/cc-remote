package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/registry"
	"github.com/yasyf/cc-remote/internal/tailnet"
	"github.com/yasyf/cc-remote/internal/workspace"
)

type selection struct {
	config   string
	provider string
	profile  string
}

func (f *selection) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.config, "config", "", "config file (default $CC_REMOTE_CONFIG or ~/.config/cc-remote/config.yaml)")
	cmd.Flags().StringVar(&f.provider, "provider", "", "provider kind from the config's providers section (default config.provider)")
	cmd.Flags().StringVar(&f.profile, "profile", "", "profile from the config's profiles section (default config.profile)")
}

func (f *selection) load() (*config.Config, error) {
	path := f.config
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if f.provider == "" {
		f.provider = cfg.Provider
	}
	if f.profile == "" {
		f.profile = cfg.Profile
	}
	return cfg, nil
}

func (f *selection) open() (*workspace.Session, error) {
	cfg, err := f.load()
	if err != nil {
		return nil, err
	}
	provider, err := openProvider(cfg, f.provider)
	if err != nil {
		return nil, err
	}
	session, err := workspace.Open(cfg, provider, f.provider, f.profile, platform(provider.Traits()))
	if err != nil {
		return nil, err
	}
	return session, nil
}

func openProvider(cfg *config.Config, kind string) (providers.Provider, error) {
	section, err := cfg.ProviderSection(kind)
	if err != nil {
		return nil, err
	}
	host, err := registry.CurrentHost(string(cfg.State()))
	if err != nil {
		return nil, err
	}
	return registry.New(kind, host, section)
}

func platform(traits providers.Traits) workspace.Platform {
	return workspace.Platform{
		Daemon: tailnet.Daemon{Mode: tailnet.Mode(traits.TailnetMode), Supervisor: tailnet.Supervisor(traits.Supervisor)},
	}
}

func emit(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newCreateCmd() *cobra.Command {
	var flags selection
	var ref, connection string
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a workspace on a fresh provider machine",
		Long: `create creates a fresh provider machine, checks out the requested ref, runs the profile's prepare steps and the
bootstrap script, enrolls the machine in the tailnet when one is configured,
and prints the connection as JSON on stdout. A failed create removes what it
made, leaving the tailnet only when this attempt enrolled.

With --connection, create runs as an Orca VM recipe: the name comes from
ORCA_RECIPE_ID and ORCA_VM_INSTANCE_ID, the checkout is pinned to
ORCA_REPO_REF_HEAD on branch ORCA_REPO_BRANCH, and stdout carries Orca's
schema 2 provisioned-root result instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := orcaMode(connection); err != nil {
				return err
			}
			name, source := "", workspace.Source{Ref: ref}
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" && connection == "" {
				return errors.New("name the workspace")
			}
			session, err := flags.open()
			if err != nil {
				return err
			}
			if connection != "" {
				if source, err = orcaSource(lookupEnv, session.Config.Repository); err != nil {
					return err
				}
				if name == "" {
					if name, err = orcaName(lookupEnv); err != nil {
						return err
					}
				}
			}
			if source.Ref == "" {
				source.Ref = session.Config.Ref
			}
			if connection == connectionServer {
				unlock, err := claimTask(session.Config.State(), name)
				if err != nil {
					return err
				}
				defer unlock()
				if err := absentTask(session.Config.State(), name); err != nil {
					return err
				}
				session.Retain = orcaRetain(session, false)
			}
			result, err := session.Create(cmd.Context(), name, source)
			if err != nil {
				return err
			}
			return emitRecipe(cmd, session, result, connection, false)
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag to check out (default config.ref)")
	bindConnection(cmd, &connection)
	return cmd
}

func bindConnection(cmd *cobra.Command, connection *string) {
	cmd.Flags().StringVar(connection, "connection", "", "run as an Orca VM recipe over this connection (ssh or server)")
}

func emitRecipe(cmd *cobra.Command, session *workspace.Session, result *workspace.Result, connection string, resumed bool) error {
	switch connection {
	case "":
		return emit(cmd.OutOrStdout(), result)
	case connectionServer:
		pairing, err := orcaServe(cmd.Context(), session, result, resumed)
		if err != nil {
			return err
		}
		return emit(cmd.OutOrStdout(), orcaServerResultOf(result, pairing))
	}
	if result.SSH == nil {
		return fmt.Errorf("%s on %s has no SSH access for an Orca ssh recipe; its recipe connects with --connection server", result.Name, result.Provider)
	}
	return emit(cmd.OutOrStdout(), orcaResultOf(result))
}

func orcaTarget(cmd *cobra.Command, args []string, connection string) (string, error) {
	if err := orcaMode(connection); err != nil {
		return "", err
	}
	if len(args) == 1 {
		return args[0], nil
	}
	if connection == "" {
		return "", errors.New("name the workspace")
	}
	return orcaResource(cmd.InOrStdin())
}

func newResumeCmd() *cobra.Command {
	var flags selection
	var connection string
	cmd := &cobra.Command{
		Use:   "resume [name]",
		Short: "Wake a workspace, refresh it, and print its connection",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := orcaTarget(cmd, args, connection)
			if err != nil {
				return err
			}
			session, err := flags.open()
			if err != nil {
				return err
			}
			if connection == connectionServer {
				unlock, err := claimTask(session.Config.State(), name)
				if err != nil {
					return err
				}
				defer unlock()
				if _, err := loadTask(session.Config.State(), name); err != nil {
					return err
				}
				session.Retain = orcaRetain(session, true)
			}
			result, err := session.Resume(cmd.Context(), name)
			if err != nil {
				return err
			}
			return emitRecipe(cmd, session, result, connection, true)
		},
	}
	flags.bind(cmd)
	bindConnection(cmd, &connection)
	return cmd
}

func newSuspendCmd() *cobra.Command {
	var flags selection
	var connection string
	cmd := &cobra.Command{
		Use:   "suspend [name]",
		Short: "Stop a workspace's compute; storage and its tailnet node stay",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := orcaTarget(cmd, args, connection)
			if err != nil {
				return err
			}
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Suspend(cmd.Context(), name)
		},
	}
	flags.bind(cmd)
	bindConnection(cmd, &connection)
	return cmd
}

func newDestroyCmd() *cobra.Command {
	var flags selection
	var connection string
	cmd := &cobra.Command{
		Use:   "destroy [name]",
		Short: "Leave the tailnet, destroy the machine, and remove its workspace record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := orcaTarget(cmd, args, connection)
			if err != nil {
				return err
			}
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Destroy(cmd.Context(), name)
		},
	}
	flags.bind(cmd)
	bindConnection(cmd, &connection)
	return cmd
}

type extended struct {
	Name     string    `json:"name"`
	Machine  string    `json:"machine"`
	Deadline time.Time `json:"deadline"`
}

func newExtendCmd() *cobra.Command {
	var flags selection
	var by time.Duration
	cmd := &cobra.Command{
		Use:   "extend <name>",
		Short: "Extend a workspace's finite provider lifetime and print the deadline the provider answered",
		Long: `extend asks the provider to let the workspace's instance live --by longer and prints the actual
deadline the provider returned, which policy may clamp. Only providers with a finite instance lifetime,
like namespace, support it; nothing renews it again on its own.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			record, err := session.Extend(cmd.Context(), args[0], by)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), extended{Name: record.Name, Machine: record.Machine, Deadline: record.Compute.Deadline})
		},
	}
	flags.bind(cmd)
	cmd.Flags().DurationVar(&by, "by", 0, "how much longer the instance may live, like 2h")
	_ = cmd.MarkFlagRequired("by")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print recorded workspaces as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			status, err := session.Status()
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), status)
		},
	}
	flags.bind(cmd)
	return cmd
}

func newVerifyCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check the config, state dir, provider login, git token and tailnet client without creating anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			checks := session.Verify(cmd.Context())
			if err := emit(cmd.OutOrStdout(), checks); err != nil {
				return err
			}
			for name, verdict := range checks {
				if verdict != "ok" {
					return fmt.Errorf("%s: %s", name, verdict)
				}
			}
			return nil
		},
	}
	flags.bind(cmd)
	return cmd
}
