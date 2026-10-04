package orca

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	RuntimeService = "cc-remote-orca"
	AgentClaude    = "claude"
	AgentCodex     = "codex"
	Loopback       = "127.0.0.1"
	runtimeDir     = remote.StateDir + "/orca"
	launcher       = runtimeDir + "/serve.sh"
	readyFile      = runtimeDir + "/serve.json"
	readyType      = "orca_server_ready"
	readySchema    = 1
	readyTries     = 240
	spriteEnv      = "/.sprite/bin/sprite-env"
	noClaudeMCP    = `{"mcpServers":{}}`
	noCodexMCP     = "{}"
)

var (
	agentValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	fableModel = regexp.MustCompile(`(?i)(^|[-_.])fable([-_.0-9]|$)`)
	keyDir     = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/\.cc-remote/orca/key\.[A-Za-z0-9]+$`)
)

type Runtime struct {
	Entry      string
	Display    []string
	Args       []string
	Port       int
	Advertise  string
	Supervised bool
}

type Ready struct {
	RuntimeID          string `json:"runtimeId"`
	BoundEndpoint      string `json:"boundEndpoint"`
	AdvertisedEndpoint string `json:"advertisedEndpoint"`
}

type Agent struct {
	Kind   string   `json:"kind"`
	Model  string   `json:"model"`
	Effort string   `json:"effort"`
	Tier   string   `json:"serviceTier,omitempty"`
	MCP    []string `json:"mcp,omitempty"`
}

const KeyDirScript = `set -eu
umask 077
mkdir -p "` + runtimeDir + `"
dir=$(mktemp -d "` + runtimeDir + `/key.XXXXXXXX")
chmod 700 "$dir"
mkfifo -m 600 "$dir/key"
printf '%s\n' "$dir"`

const ProbeScript = `set -eu
cd "` + runtimeDir + `"
exec 9>> serve.lock
if flock -n 9; then
  echo "` + remote.Prefix + `: no Orca runtime holds ` + runtimeDir + `/serve.lock" >&2
  exit 3
fi
grep '"type":"` + readyType + `"' serve.json | tail -n 1`

const FirstUseCheck = `test ! -e "` + runtimeDir + `" && test ! -L "` + runtimeDir + `"`

func (r Runtime) command() string {
	return strings.Join(slices.Concat(
		quoted(r.Display),
		[]string{`"$HOME/"` + remote.Quote(r.Entry), "serve", "--port", strconv.Itoa(r.Port), "--pairing-address", remote.Quote(cmp.Or(r.Advertise, Loopback)), "--user-data-dir", `"` + runtimeDir + `/user-data"`, "--json"},
		quoted(r.Args),
	), " ")
}

func (r Runtime) Launcher() string {
	return strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		"umask 077",
		`export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"`,
		`cd "` + runtimeDir + `"`,
		"exec 9> serve.lock",
		"flock -n 9 || exit 0",
		"exec " + r.command() + " > serve.json 2>> serve.log",
	}, "\n")
}

func (r Runtime) start() string {
	if !r.Supervised {
		return `nohup setsid "` + launcher + `" > /dev/null 2>&1 < /dev/null &`
	}
	create := spriteEnv + ` services create ` + RuntimeService + ` --cmd "` + launcher + `"`
	return strings.Join([]string{
		"if ! " + spriteEnv + " services get " + RuntimeService + " > /dev/null 2>&1; then",
		"  if " + spriteEnv + " services get cc-remote-payload > /dev/null 2>&1; then",
		"    " + create + " --needs cc-remote-payload --duration 1ms --no-stream >&2",
		"  else",
		"    " + create + " --duration 1ms --no-stream >&2",
		"  fi",
		"fi",
	}, "\n")
}

func (r Runtime) EnsureScript() string {
	return remote.Script(
		"umask 077",
		`mkdir -p "`+runtimeDir+`/user-data"`,
		`chmod 700 "`+runtimeDir+`"`,
		`test -x "$HOME/"`+remote.Quote(r.Entry)+` || { echo "`+remote.Prefix+`: $HOME/`+r.Entry+` is not an executable Orca runtime; the inventory's runtime tool provides it" >&2; exit 1; }`,
		`cat > "`+launcher+`.tmp" <<'SH'`,
		r.Launcher(),
		"SH",
		`chmod 700 "`+launcher+`.tmp"`,
		`mv "`+launcher+`.tmp" "`+launcher+`"`,
		r.start(),
		"i=0",
		`until grep -q '"type":"`+readyType+`"' "`+readyFile+`" 2> /dev/null; do`,
		`  i=$((i + 1))`,
		`  test "$i" -lt `+strconv.Itoa(readyTries)+` || { echo "`+remote.Prefix+`: the Orca runtime printed no `+readyType+` within 120s; see `+runtimeDir+`/serve.log" >&2; exit 1; }`,
		"  sleep 0.5",
		"done",
		`grep '"type":"`+readyType+`"' "`+readyFile+`" | tail -n 1`,
	)
}

func ParseReady(line []byte, listen, advertise int) (Ready, string, error) {
	var event struct {
		Type               string `json:"type"`
		SchemaVersion      int    `json:"schemaVersion"`
		RuntimeID          string `json:"runtimeId"`
		BoundEndpoint      string `json:"boundEndpoint"`
		AdvertisedEndpoint string `json:"advertisedEndpoint"`
		Pairing            struct {
			Available bool   `json:"available"`
			URL       string `json:"url"`
		} `json:"pairing"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return Ready{}, "", fmt.Errorf("decode the Orca runtime's ready event: %w", err)
	}
	advertised, err := url.Parse(event.AdvertisedEndpoint)
	bound, boundErr := url.Parse(event.BoundEndpoint)
	switch {
	case event.Type != readyType || event.SchemaVersion != readySchema:
		return Ready{}, "", fmt.Errorf("the Orca runtime printed a %q event at schema %d, not %s at schema %d", event.Type, event.SchemaVersion, readyType, readySchema)
	case event.RuntimeID == "":
		return Ready{}, "", errors.New("the Orca runtime's ready event names no runtimeId")
	case boundErr != nil || bound.Port() != strconv.Itoa(listen):
		return Ready{}, "", fmt.Errorf("the Orca runtime listens at %q, not on port %d", event.BoundEndpoint, listen)
	case err != nil || advertised.Host != net.JoinHostPort(Loopback, strconv.Itoa(advertise)):
		return Ready{}, "", fmt.Errorf("the Orca runtime advertises %q, not the forwarded ws://%s:%d", event.AdvertisedEndpoint, Loopback, advertise)
	case !event.Pairing.Available || event.Pairing.URL == "":
		return Ready{}, "", errors.New("the Orca runtime's ready event offers no pairing")
	}
	return Ready{RuntimeID: event.RuntimeID, BoundEndpoint: event.BoundEndpoint, AdvertisedEndpoint: event.AdvertisedEndpoint}, event.Pairing.URL, nil
}

func ParseKeyDir(out []byte) (string, error) {
	dir := strings.TrimSuffix(string(out), "\n")
	if !keyDir.MatchString(dir) {
		return "", fmt.Errorf("the key pipe script printed %q, not a directory under %s", dir, runtimeDir)
	}
	return dir, nil
}

func KeyWriteScript(dir string) string { return "exec cat > " + remote.Quote(dir+"/key") }

func KeyDropScript(dir string) string { return "rm -rf " + remote.Quote(dir) }

func (a Agent) Validate() error {
	if a.Kind != AgentClaude && a.Kind != AgentCodex {
		return fmt.Errorf("agent %q: use %s or %s", a.Kind, AgentClaude, AgentCodex)
	}
	if !agentValue.MatchString(a.Model) || !agentValue.MatchString(a.Effort) {
		return fmt.Errorf("model %q and effort %q must both be set to the worker's exact model and effort names", a.Model, a.Effort)
	}
	if fableModel.MatchString(a.Model) {
		return fmt.Errorf("model %q is Fable, and Fable requires an explicit local choice; a remote worker never runs it", a.Model)
	}
	if a.Tier != "" && a.Kind != AgentCodex {
		return fmt.Errorf("--service-tier is a codex setting; %s takes none", a.Kind)
	}
	if a.Tier != "" && !agentValue.MatchString(a.Tier) {
		return fmt.Errorf("--service-tier %q must be the exact tier name, such as fast", a.Tier)
	}
	if a.Kind == AgentCodex && len(a.MCP) > 1 {
		return errors.New("codex takes one --mcp-config, the TOML inline table that becomes its whole mcp_servers")
	}
	for _, server := range a.MCP {
		if server == "" || strings.HasPrefix(server, "-") {
			return fmt.Errorf("--mcp-config %q must be a config, not empty or a flag", server)
		}
	}
	return nil
}

func (a Agent) InlineMCP() error {
	form := "JSON object"
	if a.Kind == AgentCodex {
		form = "TOML table"
	}
	for i, server := range a.MCP {
		inline := strings.HasPrefix(server, "{") && strings.HasSuffix(server, "}")
		if a.Kind == AgentClaude {
			inline = inline && json.Valid([]byte(server))
		}
		if !inline {
			return fmt.Errorf("--mcp-config %d is not an inline %s; a file path would name a file on this machine, not the worker's", i+1, form)
		}
	}
	return nil
}

func (a Agent) KeyProvider() string {
	if a.Kind == AgentCodex {
		return config.KeyOpenAI
	}
	return config.KeyAnthropic
}

func (a Agent) variable() string {
	if a.Kind == AgentCodex {
		return "OPENAI_API_KEY"
	}
	return "ANTHROPIC_API_KEY"
}

func (a Agent) cleared() []string {
	if a.Kind == AgentCodex {
		return []string{"CODEX_API_KEY"}
	}
	return []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}
}

func (a Agent) Argv() []string {
	if a.Kind == AgentCodex {
		servers := noCodexMCP
		if len(a.MCP) == 1 {
			servers = a.MCP[0]
		}
		argv := []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", a.Model,
			"-c", `model_reasoning_effort="` + a.Effort + `"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", "mcp_servers=" + servers,
		}
		if a.Tier != "" {
			argv = append(argv, "-c", `service_tier="`+a.Tier+`"`)
		}
		return argv
	}
	servers := a.MCP
	if len(servers) == 0 {
		servers = []string{noClaudeMCP}
	}
	return slices.Concat(
		[]string{"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions"},
		[]string{"--strict-mcp-config", "--mcp-config"}, servers,
		[]string{"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", a.Model, "--effort", a.Effort},
	)
}

func (a Agent) Command(dir string) string {
	script := strings.Join([]string{
		"{ IFS= read -r key < " + remote.Quote(dir+"/key") + "; rm -rf " + remote.Quote(dir) + `; test -n "$key"; }`,
		"unset " + strings.Join(a.cleared(), " "),
		"export " + a.variable() + `="$key"`,
		"unset key",
		"exec " + remote.QuoteAll(a.Argv()),
	}, " && ")
	return "exec sh -c " + remote.Quote(script)
}

func quoted(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		out = append(out, remote.Quote(word))
	}
	return out
}
