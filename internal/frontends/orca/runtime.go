package orca

import (
	"bytes"
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
	claudeSettings = `{"skipDangerousModePermissionPrompt":true}`
	noCodexMCP     = "{}"
)

var (
	agentValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	fableModel = regexp.MustCompile(`(?i)(^|[-_.])fable([-_.0-9]|$)`)
	keyDir     = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/\.cc-remote/orca/key\.[A-Za-z0-9]+$`)
	sessionID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type Runtime struct {
	Entry         string
	Display       []string
	Args          []string
	Port          int
	Advertise     string
	Supervised    bool
	Restart       bool
	GoBuildJobs   int
	GoMemoryLimit int64
}

type Ready struct {
	RuntimeID          string `json:"runtimeId"`
	BoundEndpoint      string `json:"boundEndpoint"`
	AdvertisedEndpoint string `json:"advertisedEndpoint"`
	ManagedServer      string `json:"managedServer,omitempty"`
}

type Server string

const (
	ServerElectron Server = "electron"
	ServerManaged  Server = "managed"
)

type Agent struct {
	Kind     string   `json:"kind"`
	Model    string   `json:"model"`
	Effort   string   `json:"effort"`
	Tier     string   `json:"serviceTier,omitempty"`
	MCP      []string `json:"mcp,omitempty"`
	Env      []string `json:"env,omitempty"`
	Settings string   `json:"settings,omitempty"`
	Resume   string   `json:"resume,omitempty"`
}

const KeyDirScript = `set -eu
umask 077
mkdir -p "` + runtimeDir + `"
dir=$(mktemp -d "` + runtimeDir + `/key.XXXXXXXX")
chmod 700 "$dir"
mkfifo -m 600 "$dir/key"
printf '%s\n' "$dir"`

var awaitReady = []string{
	"i=0",
	`until grep -q '"type":"` + readyType + `"' "` + readyFile + `" 2> /dev/null; do`,
	`  i=$((i + 1))`,
	`  test "$i" -lt ` + strconv.Itoa(readyTries) + ` || { echo "` + remote.Prefix + `: the Orca runtime printed no ` + readyType + ` within 120s; see ` + runtimeDir + `/serve.log" >&2; exit 1; }`,
	"  sleep 0.5",
	"done",
	`grep '"type":"` + readyType + `"' "` + readyFile + `" | tail -n 1`,
}

var ProbeScript = remote.Script(slices.Concat([]string{
	`cd "` + runtimeDir + `"`,
	"exec 9>> serve.lock",
	"if flock -n 9; then",
	`  echo "` + remote.Prefix + `: no Orca runtime holds ` + runtimeDir + `/serve.lock" >&2`,
	"  exit 3",
	"fi",
}, awaitReady)...)

const FirstUseCheck = `test ! -e "` + runtimeDir + `" && test ! -L "` + runtimeDir + `"`

func (r Runtime) command() string {
	return strings.Join(slices.Concat(
		quoted(r.Display),
		[]string{`"$HOME/"` + remote.Quote(r.Entry), "serve", "--port", strconv.Itoa(r.Port), "--pairing-address", remote.Quote(cmp.Or(r.Advertise, Loopback)), "--json"},
		quoted(r.Args),
	), " ")
}

func (r Runtime) Launcher() string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"umask 077",
		`export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"`,
	}
	if r.GoBuildJobs > 0 {
		lines = append(lines, "export GOFLAGS=-p="+strconv.Itoa(r.GoBuildJobs))
	}
	if r.GoMemoryLimit > 0 {
		lines = append(lines, "export GOMEMLIMIT="+strconv.FormatInt(r.GoMemoryLimit, 10))
	}
	return strings.Join(append(lines,
		`cd "`+runtimeDir+`"`,
		"exec 9> serve.lock",
		"flock -n 9 || exit 0",
		"say() {",
		`  printf '%s\n' "$1" >&2`,
		`  printf '%s\n' "$1" >> serve.log || :`,
		"}",
		"child=",
		`trap 'kill "$child" 2> /dev/null || :; wait "$child" 2> /dev/null || :; exit 143' HUP INT TERM`,
		"delay=1",
		"while :; do",
		"  logged=0",
		"  test ! -f serve.log || logged=$(wc -c < serve.log)",
		"  started=$(date +%s)",
		"  "+r.command()+" > serve.json 2>> serve.log 9>&- &",
		"  child=$!",
		"  status=0",
		`  wait "$child" || status=$?`,
		"  test $(($(date +%s) - started)) -lt 60 || delay=1",
		"  enospc=0",
		"  tail -c +$((logged + 1)) serve.log 2> /dev/null | grep -q ENOSPC && enospc=1",
		`  full=$(df -Pk "$HOME" | awk -v enospc="$enospc" 'NR == 2 && (enospc || $4 < 65536) { printf "`+remote.Prefix+`: the Orca runtime is out of disk space: %s on %s has %d MiB free", $6, $1, $4 / 1024 }')`,
		`  test -z "$full" || say "$full"`,
		`  say "`+remote.Prefix+`: the Orca runtime exited $status; starting it again in ${delay}s"`,
		`  sleep "$delay" &`,
		"  child=$!",
		`  wait "$child"`,
		"  delay=$((delay * 2))",
		"  test $delay -le 30 || delay=30",
		"done",
	), "\n")
}

func (r Runtime) start() string {
	if !r.Supervised {
		return `nohup setsid "` + launcher + `" > /dev/null 2>&1 < /dev/null &`
	}
	create := spriteEnv + ` services create ` + RuntimeService + ` --cmd "` + launcher + `"`
	lines := []string{
		"if ! " + spriteEnv + " services get " + RuntimeService + " > /dev/null 2>&1; then",
		"  if " + spriteEnv + " services get cc-remote-payload > /dev/null 2>&1; then",
		"    " + create + " --needs cc-remote-payload --duration 1ms --no-stream >&2",
		"  else",
		"    " + create + " --duration 1ms --no-stream >&2",
		"  fi",
	}
	if r.Restart {
		lines = append(lines, "else", `  rm -f "`+readyFile+`"`, "  "+spriteEnv+" services restart "+RuntimeService+" >&2")
	}
	return strings.Join(append(lines, "fi"), "\n")
}

func (r Runtime) EnsureScript() string {
	return remote.Script(slices.Concat([]string{
		"umask 077",
		`mkdir -p "` + runtimeDir + `"`,
		`chmod 700 "` + runtimeDir + `"`,
		`test -x "$HOME/"` + remote.Quote(r.Entry) + ` || { echo "` + remote.Prefix + `: $HOME/` + r.Entry + ` is not an executable Orca runtime; the inventory's runtime tool provides it" >&2; exit 1; }`,
		`cat > "` + launcher + `.tmp" <<'SH'`,
		r.Launcher(),
		"SH",
		`chmod 700 "` + launcher + `.tmp"`,
		`mv "` + launcher + `.tmp" "` + launcher + `"`,
		r.start(),
	}, awaitReady)...)
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
		Health struct {
			BuildVersion string `json:"buildVersion"`
		} `json:"health"`
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
	return Ready{RuntimeID: event.RuntimeID, BoundEndpoint: event.BoundEndpoint, AdvertisedEndpoint: event.AdvertisedEndpoint, ManagedServer: event.Health.BuildVersion}, event.Pairing.URL, nil
}

func ServerOf(managedServer string) Server {
	if managedServer == "" {
		return ServerElectron
	}
	return ServerManaged
}

func ParseKeyDir(out []byte) (string, error) {
	dir := strings.TrimSuffix(string(out), "\n")
	if !keyDir.MatchString(dir) {
		return "", fmt.Errorf("the key pipe script printed %q, not a directory under %s", dir, runtimeDir)
	}
	return dir, nil
}

const typesafeVariable = "TYPESAFE_API_KEY"

const trunkVariable = "TRUNK_MERGE_API_KEY"

const geminiVariable = "GEMINI_API_KEY"

const (
	memoryDir         = "/dev/shm/cc-remote"
	githubAppVariable = "CCX_GITHUB_APP_TOKEN_FILE"
	githubAppFile     = memoryDir + "/github-app.json"
	awsVariable       = "AWS_CONFIG_FILE"
	awsFile           = memoryDir + "/aws.json"
	awsConfigFile     = memoryDir + "/aws-config"
)

var (
	GitHubAppScript = memoryScript(githubAppFile)
	AWSScript       = memoryScript(awsFile)
)

func memoryScript(file string) string {
	return `umask 077 && mkdir -p ` + memoryDir + ` && tmp=$(mktemp ` + file + `.XXXXXX) && cat > "$tmp" && mv "$tmp" ` + file
}

var githubVariables = []string{"GH_TOKEN", "GITHUB_TOKEN"}

var envAssignment = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)=[A-Za-z0-9+/=:._@-]+$`)

func EnvLine(out []byte) ([]byte, error) {
	owned := slices.Concat(CredentialEnv(), []string{trunkVariable, geminiVariable, githubAppVariable, awsVariable}, githubVariables)
	var assignments [][]byte
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
		match := envAssignment.FindSubmatch(line)
		if match == nil {
			return nil, errors.New("must print NAME=value lines whose values hold no spaces, quotes or shell characters")
		}
		if name := string(match[1]); slices.Contains(owned, name) || strings.HasPrefix(name, "GIT_CONFIG_") {
			return nil, fmt.Errorf("sets %s, which another orca.keys command or the launcher's git identity sets", name)
		}
		assignments = append(assignments, line)
	}
	return bytes.Join(assignments, []byte{' '}), nil
}

const authorIdentity = `export GIT_CONFIG_COUNT=2 ` +
	`GIT_CONFIG_KEY_0=user.name GIT_CONFIG_VALUE_0="$name" ` +
	`GIT_CONFIG_KEY_1=user.email GIT_CONFIG_VALUE_1="$email"`

const githubCredentials = `export GH_TOKEN="$github" GIT_CONFIG_COUNT=4 ` +
	`GIT_CONFIG_KEY_2=credential.https://github.com.helper GIT_CONFIG_VALUE_2= ` +
	`GIT_CONFIG_KEY_3=credential.https://github.com.helper 'GIT_CONFIG_VALUE_3=!gh auth git-credential'`

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
	if a.Settings != "" && a.Kind != AgentClaude {
		return fmt.Errorf("--settings is a claude setting; %s takes none", a.Kind)
	}
	if a.Settings != "" {
		var settings map[string]any
		if err := json.Unmarshal([]byte(a.Settings), &settings); err != nil || settings == nil {
			return fmt.Errorf("--settings %q is not a JSON object, and claude drops an invalid --settings whole", a.Settings)
		}
	}
	if a.Resume != "" && a.Kind != AgentClaude {
		return fmt.Errorf("--resume is a claude setting; %s takes none", a.Kind)
	}
	if a.Resume != "" && !sessionID.MatchString(a.Resume) {
		return fmt.Errorf("--resume %q is not a Claude session id", a.Resume)
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

func CredentialEnv() []string {
	var names []string
	for _, agent := range []Agent{{Kind: AgentClaude}, {Kind: AgentCodex}} {
		names = append(append(names, agent.variable()), agent.cleared()...)
	}
	return append(names, typesafeVariable)
}

func (a Agent) Argv(policy ShellPolicy) []string {
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
			"-c", policy.override(),
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
	argv := slices.Concat(
		[]string{"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", a.settings()},
		[]string{"--strict-mcp-config", "--mcp-config"}, servers,
		[]string{"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", a.Model, "--effort", a.Effort},
	)
	if a.Resume != "" {
		argv = append(argv, "--resume", a.Resume)
	}
	return argv
}

func (a Agent) settings() string {
	if a.Settings == "" {
		return claudeSettings
	}
	merged := map[string]any{}
	_ = json.Unmarshal([]byte(a.Settings), &merged)
	_ = json.Unmarshal([]byte(claudeSettings), &merged)
	encoded, _ := json.Marshal(merged)
	return string(encoded)
}

func (a Agent) exports() []string {
	exports := append([]string{"CAPT_HOOK_ACTOR_JUDGE=" + remote.Quote(a.Kind)}, quoted(a.Env)...)
	if a.Kind == AgentCodex {
		return exports
	}
	return append(exports, "DISABLE_AUTOUPDATER=1")
}

func (a Agent) judge() Agent {
	if a.Kind == AgentCodex {
		return Agent{Kind: AgentClaude}
	}
	return Agent{Kind: AgentCodex}
}

func (a Agent) JudgeKeyProvider() string {
	return a.judge().KeyProvider()
}

func (a Agent) Command(dir string, policy ShellPolicy) string {
	judge := a.judge()
	script := strings.Join([]string{
		"{ { IFS= read -r key; IFS= read -r judge || judge=; IFS= read -r typesafe || typesafe=; IFS= read -r github || github=; IFS= read -r name; IFS= read -r email; IFS= read -r trunk || trunk=; IFS= read -r gemini || gemini=; IFS= read -r login || login=; IFS= read -r environment || environment=; IFS= read -r aws || aws=; } < " + remote.Quote(dir+"/key") + "; rm -rf " + remote.Quote(dir) + `; test -n "$key"; }`,
		"unset " + strings.Join(slices.Concat(a.cleared(), []string{judge.variable()}, judge.cleared(), []string{typesafeVariable, trunkVariable, geminiVariable, awsVariable}, githubVariables), " "),
		"export " + a.variable() + `="$key"`,
		`{ test -z "$judge" || export ` + judge.variable() + `="$judge"; }`,
		`{ test -z "$typesafe" || export ` + typesafeVariable + `="$typesafe"; }`,
		authorIdentity,
		`{ test -z "$github" || ` + githubCredentials + `; }`,
		`{ test -z "$trunk" || export ` + trunkVariable + `="$trunk"; }`,
		`{ test -z "$gemini" || export ` + geminiVariable + `="$gemini"; }`,
		`{ test -z "$login" || gh config set -h github.com user "$login"; }`,
		`{ test -z "$environment" || export $environment; }`,
		"export " + githubAppVariable + "=" + githubAppFile,
		`{ test -z "$aws" || { printf '%s' "$aws" | (` + AWSScript + `) && printf '[default]\ncredential_process = cat ` + awsFile + `\n' > ` + awsConfigFile + ` && export ` + awsVariable + `=` + awsConfigFile + `; }; }`,
		"unset key judge typesafe github trunk gemini name email login environment aws",
		"export " + strings.Join(a.exports(), " "),
		"exec " + remote.QuoteAll(a.Argv(policy)),
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
