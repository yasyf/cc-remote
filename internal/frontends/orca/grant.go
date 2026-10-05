package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type GrantMode string

const (
	GrantFill     GrantMode = "fill"
	GrantClaim    GrantMode = "claim"
	GrantFinal    GrantMode = "final"
	grantSchema             = 1
	grantDeadline           = 30 * time.Second
	grantSlack              = 15 * time.Second
)

const appServerLibrary = `
import re
import selectors
import shutil
import subprocess
import time

HASH = re.compile(r"sha256:[0-9a-f]{64}")
LIMIT = 16 << 20
GRACE = 5


class Server:
    def __init__(self, binary, env, home, deadline):
        self.deadline = deadline
        self.child = subprocess.Popen([binary, "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, cwd=home)
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.child.stdout, selectors.EVENT_READ)
        self.buffer = b""
        self.size = 0
        self.ident = 0

    def send(self, message, step):
        try:
            self.child.stdin.write(json.dumps(message).encode() + b"\n")
            self.child.stdin.flush()
        except OSError:
            raise Refusal("exit", step=step)

    def notify(self, method):
        self.send({"method": method}, method)

    def request(self, method, params):
        self.ident += 1
        self.send({"id": self.ident, "method": method, "params": params}, method)
        while True:
            line, found, rest = self.buffer.partition(b"\n")
            if found:
                self.buffer = rest
                result = self.decode(line, method)
                if result is not None:
                    return result
                continue
            left = self.deadline - time.monotonic()
            if left <= 0 or not self.selector.select(left):
                raise Refusal("timeout", step=method)
            chunk = os.read(self.child.stdout.fileno(), 65536)
            if not chunk:
                raise Refusal("exit", step=method)
            self.size += len(chunk)
            if self.size > LIMIT:
                raise Refusal("protocol", step=method)
            self.buffer += chunk

    def decode(self, line, method):
        try:
            message = json.loads(line)
        except ValueError:
            raise Refusal("protocol", step=method)
        if not isinstance(message, dict):
            raise Refusal("protocol", step=method)
        if "id" not in message:
            return None
        if "method" in message or message["id"] != self.ident:
            raise Refusal("protocol", step=method)
        if "error" in message:
            raise Refusal("rpc", step=method)
        if not isinstance(message.get("result"), dict):
            raise Refusal("protocol", step=method)
        return message["result"]

    def close(self):
        try:
            self.child.stdin.close()
        except OSError:
            pass
        try:
            return self.child.wait(timeout=max(0, min(GRACE, self.deadline - time.monotonic())))
        except subprocess.TimeoutExpired:
            self.child.kill()
            self.child.wait()
            return None
`

const HookGrantScript = hookLibrary + appServerLibrary + `

def segment(value):
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def trust_level(config, project):
    projects = config.get("projects", {})
    entry = projects.get(project, {}) if isinstance(projects, dict) else None
    if not isinstance(entry, dict):
        raise Refusal("user-config")
    return entry.get("trust_level")


def project_trust(result, home, project):
    layers = result.get("layers")
    if not isinstance(layers, list) or not all(isinstance(layer, dict) and isinstance(layer.get("name"), dict) and isinstance(layer.get("config"), dict) for layer in layers):
        raise Refusal("user-config")
    users = [layer for layer in layers if layer["name"].get("type") == "user"]
    if len(users) != 1:
        raise Refusal("user-config")
    user = users[0]
    if user["name"].get("file") != home + "/.codex/config.toml" or user["name"].get("profile") is not None or user.get("disabledReason") is not None or not isinstance(user.get("version"), str):
        raise Refusal("user-config")
    if any(trust_level(layer["config"], project) not in (None, "trusted") for layer in layers):
        raise Refusal("project-untrusted")
    return trust_level(user["config"], project) == "trusted", user["version"]


def written(result):
    if result.get("status") != "ok":
        raise Refusal("overridden", step="config/batchWrite")
    if not isinstance(result.get("version"), str):
        raise Refusal("protocol", step="config/batchWrite")
    return result["version"]


def listing(result, project, home, handlers):
    data = result.get("data")
    if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict):
        raise Refusal("listing")
    entry = data[0]
    if entry.get("cwd") != project or entry.get("errors") != [] or not isinstance(entry.get("hooks"), list):
        raise Refusal("listing")
    names = {event[0].lower() + event[1:]: event for event, _ in handlers}
    remaining = [(event[0].lower() + event[1:], handler["command"], handler.get("timeout", 600)) for event, handler in handlers]
    found = []
    for index, hook in enumerate(entry["hooks"]):
        if not isinstance(hook, dict):
            raise Refusal("listing", record=index)
        event = names.get(hook.get("eventName"), "")
        identity = (hook.get("eventName"), hook.get("command"), hook.get("timeoutSec"))
        native = (hook.get("source"), hook.get("sourcePath"), hook.get("isManaged"), hook.get("pluginId"), hook.get("handlerType"), hook.get("async"), hook.get("matcher"), hook.get("statusMessage"), hook.get("additionalContextLimit"))
        if native != ("user", home + "/.codex/hooks.json", False, None, "command", False, None, None, None) or identity not in remaining:
            raise Refusal("extra", event, index)
        remaining.remove(identity)
        key, digest, status = hook.get("key"), hook.get("currentHash"), hook.get("trustStatus")
        if not isinstance(key, str) or not key or any(prior["key"] == key for prior in found):
            raise Refusal("duplicate", event, index)
        if not isinstance(digest, str) or not HASH.fullmatch(digest):
            raise Refusal("listing", event, index)
        if hook.get("enabled") is not True:
            raise Refusal("disabled", event, index)
        if status == "modified":
            raise Refusal("modified", event, index)
        if status not in ("trusted", "untrusted"):
            raise Refusal("listing", event, index)
        found.append({"key": key, "hash": digest, "trusted": status == "trusted", "event": event, "record": index, "identity": identity})
    if remaining:
        raise Refusal("missing", names[remaining[0][0]])
    return found


def grant(mode, plugin, version, codex, project, seconds, excluded):
    started = time.monotonic()
    deadline = started + seconds
    timings = {}

    def mark(name, since):
        timings[name] = round(time.monotonic() - since, 6)
        return time.monotonic()

    if os.environ.get("CODEX_HOME", os.path.expanduser("~") + "/.codex") != os.path.expanduser("~") + "/.codex":
        raise Refusal("home")
    if not os.path.isabs(project) or os.path.realpath(project) != project or not os.path.isdir(project):
        raise Refusal("project")
    home, root, raw, events, handlers = definitions(plugin, version, mode == "final")
    env = {key: value for key, value in os.environ.items() if key not in excluded}
    env["PATH"] = home + "/.local/bin:/usr/local/bin:" + env.get("PATH", "")
    binary = shutil.which("codex", path=env["PATH"])
    if binary is None:
        raise Refusal("codex")
    binary = os.path.realpath(binary)
    try:
        answer = subprocess.run([binary, "--version"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=max(0.001, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise Refusal("timeout", step="version")
    if answer.returncode != 0 or answer.stdout.decode(errors="replace").strip() != "codex-cli " + codex:
        raise Refusal("codex")
    since = mark("version", started)
    project_write, writes = False, 0
    server = Server(binary, env, home, deadline)
    try:
        hello = server.request("initialize", {"clientInfo": {"name": "cc_remote", "title": "cc-remote", "version": "1"}})
        if hello.get("codexHome") != home + "/.codex":
            raise Refusal("home", step="initialize")
        server.notify("initialized")
        since = mark("initialize", since)
        trusted, current = project_trust(server.request("config/read", {"includeLayers": True}), home, project)
        if not trusted and mode != "fill":
            raise Refusal("project-untrusted")
        if not trusted:
            current = written(server.request("config/batchWrite", {"edits": [{"keyPath": "projects." + segment(project) + ".trust_level", "value": "trusted", "mergeStrategy": "replace"}], "expectedVersion": current, "reloadUserConfig": True}))
            project_write = True
        since = mark("project", since)
        listed = listing(server.request("hooks/list", {"cwds": [project]}), project, home, handlers)
        since = mark("list", since)
        pending = [hook for hook in listed if not hook["trusted"]]
        if pending and mode != "fill":
            raise Refusal("untrusted-hook", pending[0]["event"], pending[0]["record"])
        if pending:
            written(server.request("config/batchWrite", {"edits": [{"keyPath": "hooks.state", "value": {hook["key"]: {"trusted_hash": hook["hash"]} for hook in pending}, "mergeStrategy": "upsert"}], "expectedVersion": current, "reloadUserConfig": True}))
            writes = len(pending)
            since = mark("grant", since)
            verified = listing(server.request("hooks/list", {"cwds": [project]}), project, home, handlers)
            if [(hook["key"], hook["hash"], hook["identity"]) for hook in verified] != [(hook["key"], hook["hash"], hook["identity"]) for hook in listed]:
                raise Refusal("changed")
            untrusted = [hook for hook in verified if not hook["trusted"]]
            if untrusted:
                raise Refusal("untrusted-hook", untrusted[0]["event"], untrusted[0]["record"])
            since = mark("verify", since)
    finally:
        code = server.close()
    if code is None:
        raise Refusal("timeout", step="exit")
    if code != 0:
        raise Refusal("exit", step="exit")
    mark("exit", since)
    mark("total", started)
    hooks = sorted(({"key": hook["key"], "hash": hook["hash"]} for hook in listed), key=lambda hook: hook["key"])
    return {"schema": 1, "mode": mode, "outcome": "exact", "reason": "", "event": "", "record": -1, "step": "", "home": home, "root": root, "project": project, "hooksSha256": hashlib.sha256(raw).hexdigest(), "definitions": sum(events.values()), "events": events, "projectWrite": project_write, "hookWrites": writes, "hooks": hooks, "seconds": timings}


def main():
    mode = sys.argv[1]
    try:
        result = grant(mode, sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], float(sys.argv[6]), set(sys.argv[7:]))
    except Refusal as refusal:
        result = {"schema": 1, "mode": mode, "outcome": "refused", "reason": refusal.reason, "event": refusal.event, "record": refusal.record, "step": refusal.step}
    except Exception:
        result = {"schema": 1, "mode": mode, "outcome": "refused", "reason": "grant-error", "event": "", "record": -1, "step": ""}
    print(json.dumps(result, sort_keys=True))


main()
`

var (
	ErrPregrantPrompt = errors.New("the pregranted Codex worker asked for folder trust or hook review, which a pregrant never answers")
	grantRefusals     = append(slices.Clone(probeRefusals), "project", "codex", "timeout", "exit", "protocol", "rpc", "user-config", "project-untrusted", "overridden", "listing", "extra", "duplicate", "disabled", "modified", "untrusted-hook", "changed", "grant-error")
	grantSteps        = []string{"", "version", "initialize", "config/read", "config/batchWrite", "hooks/list", "exit"}
	grantTimings      = []string{"version", "initialize", "project", "list", "grant", "verify", "exit", "total"}
	trustHash         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type HookGrant struct {
	Captain []Pin
	Codex   string
	Project string
	Exec    func(ctx context.Context, argv []string) ([]byte, error)
}

type GrantedHook struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

type Pregrant struct {
	Project string        `json:"project"`
	Hooks   []GrantedHook `json:"hooks"`
}

type HookGrantResult struct {
	Schema       int                `json:"schema"`
	Mode         GrantMode          `json:"mode"`
	Outcome      string             `json:"outcome"`
	Reason       string             `json:"reason"`
	Event        string             `json:"event"`
	Record       int                `json:"record"`
	Step         string             `json:"step"`
	Home         string             `json:"home"`
	Root         string             `json:"root"`
	Project      string             `json:"project"`
	HooksSHA256  string             `json:"hooksSha256"`
	Definitions  int                `json:"definitions"`
	Events       map[string]int     `json:"events"`
	ProjectWrite bool               `json:"projectWrite"`
	HookWrites   int                `json:"hookWrites"`
	Hooks        []GrantedHook      `json:"hooks"`
	Seconds      map[string]float64 `json:"seconds"`
}

type GrantRefusal struct {
	Mode   GrantMode
	Reason string
	Event  string
	Record int
	Step   string
}

type GrantedHooks struct {
	Grant HookGrant
	Prior Pregrant
}

func GrantedCodexStartup(grant HookGrant, prior Pregrant) Startup {
	return Startup{Gates: []Gate{folderTrust}, Granted: &GrantedHooks{Grant: grant, Prior: prior}}
}

func (e GrantRefusal) Error() string {
	where := ""
	if e.Event != "" {
		where += fmt.Sprintf(" at %s record %d", e.Event, e.Record)
	}
	if e.Step != "" {
		where += " during " + e.Step
	}
	return fmt.Sprintf("the %s grant of the worker's Codex trust refused: %s%s", e.Mode, e.Reason, where)
}

func (g HookGrant) Run(ctx context.Context, mode GrantMode) (result HookGrantResult, err error) {
	started := time.Now()
	defer func() {
		observe(ctx, "hooks.grant", started, err, "mode", mode, "projectWrite", result.ProjectWrite, "hookWrites", result.HookWrites, "sessionSeconds", result.Seconds["total"])
	}()
	pin, err := HookReview{Captain: g.Captain}.captain()
	if err != nil {
		return HookGrantResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, grantDeadline+grantSlack)
	defer cancel()
	argv := append([]string{"python3", "-c", HookGrantScript, string(mode), pin.ID, pin.Version, g.Codex, g.Project, strconv.Itoa(int(grantDeadline.Seconds()))}, CredentialEnv()...)
	out, err := g.Exec(ctx, argv)
	if err != nil {
		return HookGrantResult{}, fmt.Errorf("run the %s grant of the worker's Codex trust: %w", mode, err)
	}
	return ParseHookGrant(out, pin, mode, g.Project)
}

func (g HookGrant) Fill(ctx context.Context) (*Pregrant, error) {
	result, err := g.Run(ctx, GrantFill)
	if err != nil {
		return nil, err
	}
	return &Pregrant{Project: result.Project, Hooks: result.Hooks}, nil
}

func (g HookGrant) Claim(ctx context.Context, prior Pregrant) error {
	result, err := g.Run(ctx, GrantClaim)
	switch {
	case err != nil:
		return err
	case prior.Project != g.Project || !slices.Equal(result.Hooks, prior.Hooks):
		return errors.New("the claimed workspace's Codex hooks are not the ones its pregrant trusted")
	}
	return nil
}

func (g HookGrant) Final(ctx context.Context, prior Pregrant) error {
	result, err := g.Run(ctx, GrantFinal)
	if err != nil {
		return err
	}
	hashes := map[string]bool{}
	for _, hook := range result.Hooks {
		hashes[hook.Hash] = true
	}
	for _, hook := range prior.Hooks {
		if !hashes[hook.Hash] {
			return errors.New("a pregranted Captain Hook handler lost its trusted definition after Orca installed its handlers")
		}
	}
	return nil
}

func ParseHookGrant(out []byte, pin Pin, mode GrantMode, project string) (HookGrantResult, error) {
	var result HookGrantResult
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.More() {
		return HookGrantResult{}, errors.New("the Codex trust grant printed no single result document")
	}
	if result.Schema != grantSchema || result.Mode != mode {
		return HookGrantResult{}, errors.New("the Codex trust grant answered with another schema or mode")
	}
	switch result.Outcome {
	case "refused":
		if !slices.Contains(grantRefusals, result.Reason) || (result.Event != "" && !slices.Contains(nativeHookEvents, result.Event)) || !slices.Contains(grantSteps, result.Step) {
			return HookGrantResult{}, errors.New("the Codex trust grant refused for an unrecognized reason")
		}
		return HookGrantResult{}, GrantRefusal{Mode: mode, Reason: result.Reason, Event: result.Event, Record: result.Record, Step: result.Step}
	case "exact":
	default:
		return HookGrantResult{}, errors.New("the Codex trust grant reported an unrecognized outcome")
	}
	counts := map[string]int{}
	for _, event := range captainHookEvents {
		counts[event] = 1
	}
	writable := len(captainHookEvents)
	if mode == GrantFinal {
		counts = multiplicity()
	}
	if mode != GrantFill {
		writable = 0
	}
	name, market, _ := strings.Cut(pin.ID, "@")
	switch {
	case !path.IsAbs(result.Home) || path.Clean(result.Home) != result.Home:
		return HookGrantResult{}, errors.New("the Codex trust grant reported no absolute runtime home")
	case result.Root != path.Join(result.Home, ".claude/plugins/cache", market, name, pin.Version):
		return HookGrantResult{}, errors.New("the Codex trust grant matched another Captain Hook root")
	case result.Project != project:
		return HookGrantResult{}, errors.New("the Codex trust grant answered for another project")
	case !sha256Hex.MatchString(result.HooksSHA256):
		return HookGrantResult{}, errors.New("the Codex trust grant reported no hooks digest")
	case !maps.Equal(result.Events, counts) || result.Definitions != len(result.Hooks) || result.Definitions != definitionCount(counts):
		return HookGrantResult{}, errors.New("the Codex trust grant counted another set of definitions")
	case result.HookWrites < 0 || result.HookWrites > writable || (result.ProjectWrite && mode != GrantFill):
		return HookGrantResult{}, errors.New("the Codex trust grant wrote more than its mode allows")
	}
	for i, hook := range result.Hooks {
		if hook.Key == "" || !trustHash.MatchString(hook.Hash) || (i > 0 && result.Hooks[i-1].Key >= hook.Key) {
			return HookGrantResult{}, errors.New("the Codex trust grant reported a hook without a unique key and native hash")
		}
	}
	for name := range result.Seconds {
		if !slices.Contains(grantTimings, name) {
			return HookGrantResult{}, errors.New("the Codex trust grant reported an unrecognized timing")
		}
	}
	return result, nil
}

func definitionCount(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}
