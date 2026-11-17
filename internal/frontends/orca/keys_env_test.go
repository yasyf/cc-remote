package orca_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func TestKeysEnvExportsWhatALaunchWouldAndSourcesInAShell(t *testing.T) {
	env := orca.Agent{Kind: orca.AgentClaude}.KeysEnv(orca.Keys{
		Key: []byte("sk-'quoted"), TypeSafe: []byte("ts"), GitHub: []byte("gh"), Name: []byte("Ada Lovelace"),
		Email: []byte("ada@example.com"), Env: []byte("AWS_REGION=us-west-2 EXTRA=1"), GitHubApp: true, AWS: true,
	})
	script := string(env) + `printf '%s|' "$ANTHROPIC_API_KEY" "$TYPESAFE_API_KEY" "$GH_TOKEN" "${OPENAI_API_KEY-unset}" "$GIT_CONFIG_COUNT" "$GIT_CONFIG_VALUE_0" "$GIT_CONFIG_VALUE_2" "$GIT_CONFIG_VALUE_3" "$AWS_REGION" "$EXTRA" "$CCX_GITHUB_APP_TOKEN_FILE" "$AWS_CONFIG_FILE"`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "sk-'quoted|ts|gh|unset|4|Ada Lovelace||!gh auth git-credential|us-west-2|1|" + orca.GitHubAppFile + "|/dev/shm/cc-remote/aws-config|"
	if string(out) != want {
		t.Fatalf("sourced = %q, want %q", out, want)
	}
	if !strings.HasPrefix(string(env), "unset CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_AUTH_TOKEN CODEX_API_KEY\n") {
		t.Fatalf("env = %q, want it to clear the credentials that outrank the API key first", env)
	}
}
