package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
)

type pushed struct {
	Machine string     `json:"machine"`
	Expires *time.Time `json:"expires,omitempty"`
}

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Hand a machine the keys its commands read"}
	cmd.AddCommand(newKeysPushCmd())
	return cmd
}

func newKeysPushCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "push <machine>",
		Short: "Capture every orca.keys key on this machine and write them to the remote machine's memory for its steps",
		Long: `push runs each orca.keys command here, as a launch captures it, and writes what the commands
print to /dev/shm/cc-remote on the machine, never to its disk: the exports every step sources before its
command, the AWS session and the GitHub App token. It prints JSON naming the machine and, when a key
expires, the earliest expiry, so the caller pushes again before then; a second push replaces the first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			provider, err := openProvider(cfg, flags.provider)
			if err != nil {
				return err
			}
			launcher, err := captureAuthor(cmd.Context())
			if err != nil {
				return err
			}
			expires, err := pushKeys(cmd.Context(), provider, cfg.State(), cfg.Orca.Keys, launcher, args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(pushed{Machine: args[0], Expires: expires})
		},
	}
	flags.bind(cmd)
	return cmd
}

func pushKeys(ctx context.Context, provider providers.Provider, dir state.Dir, keys map[string][]string, launcher author, machine string) (*time.Time, error) {
	agent := orca.Agent{Kind: orca.AgentClaude}
	captured, err := captureKeys(ctx, dir, keys, agent)
	if err != nil {
		return nil, err
	}
	defer captured.clear()
	var expires *time.Time
	earliest := func(at time.Time) {
		if expires == nil || at.Before(*expires) {
			expires = &at
		}
	}
	if len(captured.aws) > 0 {
		var session struct{ Expiration time.Time }
		if err := json.Unmarshal(captured.aws, &session); err != nil {
			return nil, err
		}
		earliest(session.Expiration)
		if err := pushScript(ctx, provider, machine, orca.AWSConfigScript, captured.aws); err != nil {
			return nil, fmt.Errorf("aws session: %w", err)
		}
	}
	if command, ok := keys[config.KeyGitHubApp]; ok {
		token, err := captureKey(ctx, dir, command)
		if err != nil {
			return nil, fmt.Errorf("github_app %w", err)
		}
		defer clear(token)
		var app struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(token, &app); err != nil {
			return nil, fmt.Errorf("github_app command must print the token JSON with its expires_at: %w", err)
		}
		earliest(app.ExpiresAt)
		if err := pushScript(ctx, provider, machine, orca.GitHubAppScript, token); err != nil {
			return nil, fmt.Errorf("github app token: %w", err)
		}
	}
	if len(captured.githubLogin) > 0 {
		if err := pushExec(ctx, provider, machine, []string{"gh", "config", "set", "-h", "github.com", "user", string(captured.githubLogin)}, nil); err != nil {
			return nil, fmt.Errorf("github_login: %w", err)
		}
	}
	env := agent.KeysEnv(orca.Keys{
		Key: captured.key, Judge: captured.judge, TypeSafe: captured.typesafe, GitHub: captured.github,
		Name: []byte(launcher.name), Email: []byte(launcher.email), Trunk: captured.trunk, Gemini: captured.gemini,
		Env: captured.env, GitHubApp: len(captured.githubApp) > 0, AWS: len(captured.aws) > 0,
	})
	defer clear(env)
	return expires, pushScript(ctx, provider, machine, orca.KeysScript, env)
}

func pushScript(ctx context.Context, provider providers.Provider, machine, script string, stdin []byte) error {
	return pushExec(ctx, provider, machine, []string{"sh", "-c", script}, bytes.NewReader(stdin))
}

func pushExec(ctx context.Context, provider providers.Provider, machine string, argv []string, stdin io.Reader) error {
	result, err := provider.Exec(ctx, machine, argv, stdin)
	if err == nil && result.ExitCode != 0 {
		err = &providers.CommandError{Command: argv[0] + " on " + machine, Result: result}
	}
	return err
}
