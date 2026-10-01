package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

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
	session.Refill = func() error { return refill(cfg, f.provider, f.profile, session) }
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
		Daemon:           tailnet.Daemon{Mode: tailnet.Mode(traits.TailnetMode), Supervisor: tailnet.Supervisor(traits.Supervisor)},
		HostKeys:         traits.HostKeys,
		CredentialHelper: traits.CredentialHelper,
	}
}

func refill(cfg *config.Config, provider, profile string, session *workspace.Session) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	argv := []string{self, "prepare", "--config", cfg.Path, "--provider", provider, "--profile", profile}
	_, err = workspace.StartDetached(argv, session.State.Log("prepare", provider+"-"+profile))
	return err
}

func emit(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newCreateCmd() *cobra.Command {
	var flags selection
	var ref string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a workspace, claiming a prepared spare when one matches",
		Long: `create claims a ready spare with the current fingerprint or creates a fresh
machine, checks out the requested ref, runs the profile's prepare steps and the
bootstrap script, enrolls the machine in the tailnet when one is configured,
and prints the connection as JSON on stdout. A failed create removes what it
made, leaving the tailnet only when this attempt enrolled.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			if ref == "" {
				ref = session.Config.Ref
			}
			result, err := session.Create(cmd.Context(), args[0], ref)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), result)
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag to check out (default config.ref)")
	return cmd
}

func newResumeCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "resume <name>",
		Short: "Wake a workspace, refresh it, and print its connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			result, err := session.Resume(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), result)
		},
	}
	flags.bind(cmd)
	return cmd
}

func newSuspendCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "suspend <name>",
		Short: "Stop a workspace's compute; storage and its tailnet node stay",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Suspend(cmd.Context(), args[0])
		},
	}
	flags.bind(cmd)
	return cmd
}

func newDestroyCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Leave the tailnet, destroy the machine, and retire the ledger entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Destroy(cmd.Context(), args[0])
		},
	}
	flags.bind(cmd)
	return cmd
}

func newPrepareCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:     "prepare",
		Aliases: []string{"warm"},
		Short:   "Prepare spares until the pool is full or the budget refuses one",
		Long: `prepare creates machines with the profile's tools, a shallow checkout of the
config ref and its warm steps, checks that each holds no credential, suspends
it, and marks it ready. A spare never joins the tailnet; create enrolls it at
claim time with a fresh identity. Stale spares are destroyed first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Prepare(cmd.Context())
		},
	}
	flags.bind(cmd)
	return cmd
}

func newDrainCmd() *cobra.Command {
	var flags selection
	var all bool
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Destroy unclaimed spares; stale ones by default, every one with --all",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			return session.Drain(cmd.Context(), all)
		},
	}
	flags.bind(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "destroy current spares too")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print workspaces, the spare pool and the budget as JSON",
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
