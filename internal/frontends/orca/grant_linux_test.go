package orca_test

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const fakeAppServer = `#!/usr/bin/env python3
import hashlib
import json
import os
import sys
import time
import tomllib

here = os.path.dirname(os.path.realpath(__file__))
with open(os.path.join(here, "scenario.json")) as source:
    scenario = json.load(source)
home = os.environ["HOME"]
with open(os.path.join(here, "invocations"), "a") as marker:
    marker.write(" ".join(sys.argv[1:]) + "\n")
if sys.argv[1:] == ["--version"]:
    print(scenario.get("version", "codex-cli 0.159.2"))
    sys.exit(0)
log = open(os.path.join(here, "requests.jsonl"), "a")
log.write(json.dumps({"env": sorted(os.environ)}) + "\n")
log.flush()
state_path = os.path.join(here, "state.json")
if os.path.exists(state_path):
    with open(state_path) as source:
        state = json.load(source)
else:
    with open(home + "/.codex/config.toml", "rb") as source:
        state = {"config": tomllib.load(source), "version": 1}


def version():
    return "sha256:%064x" % state["version"]


def segments(path):
    out, segment, quoted, chars = [], "", False, iter(path)
    for ch in chars:
        if ch == '"' and not segment and not quoted:
            quoted = True
        elif ch == '"' and quoted:
            quoted = False
        elif ch == "\\" and quoted:
            segment += next(chars)
        elif ch == "." and not quoted:
            out.append(segment)
            segment = ""
        else:
            segment += ch
    return out + [segment]


def merge(base, overlay):
    for key, value in overlay.items():
        if isinstance(base.get(key), dict) and isinstance(value, dict):
            merge(base[key], value)
        else:
            base[key] = value


def apply(edit):
    path = segments(edit["keyPath"])
    node = state["config"]
    for segment in path[:-1]:
        node = node.setdefault(segment, {})
    last = path[-1]
    if edit["mergeStrategy"] == "upsert" and isinstance(node.get(last), dict) and isinstance(edit["value"], dict):
        merge(node[last], edit["value"])
    else:
        node[last] = edit["value"]


def listing(cwd, call):
    source = home + "/.codex/hooks.json"
    with open(source) as handle:
        document = json.load(handle)
    hooks = []
    for event, groups in document["hooks"].items():
        label = "".join("_" + c.lower() if c.isupper() else c for c in event).lstrip("_")
        camel = event[0].lower() + event[1:]
        for g, group in enumerate(groups):
            for h, handler in enumerate(group["hooks"]):
                key = "%s:%s:%d:%d" % (source, label, g, h)
                identity = json.dumps({"event": label, "matcher": group.get("matcher"), "handler": handler}, sort_keys=True)
                digest = "sha256:" + hashlib.sha256(identity.encode()).hexdigest()
                entry = state["config"].get("hooks", {}).get("state", {}).get(key, {})
                trusted = entry.get("trusted_hash")
                status = "untrusted" if trusted is None else ("trusted" if trusted == digest else "modified")
                hook = {"key": key, "currentHash": digest, "trustStatus": status, "enabled": entry.get("enabled") is not False, "source": "user", "sourcePath": source, "isManaged": False, "pluginId": None, "eventName": camel, "matcher": group.get("matcher"), "statusMessage": handler.get("statusMessage"), "timeoutSec": handler.get("timeout", 600), "handlerType": handler["type"], "command": handler["command"], "async": handler.get("async", False), "displayOrder": len(hooks), "additionalContextLimit": None}
                hook.update(scenario.get("patch", {}).get(camel, {}))
                hook.update(scenario.get("patchCall", {}).get(str(call), {}).get(camel, {}))
                if camel not in scenario.get("drop", []):
                    hooks.append(hook)
    return {"data": [{"cwd": cwd, "errors": [], "warnings": [], "hooks": hooks + scenario.get("extra", [])}]}


def reply(message):
    print(json.dumps(message), flush=True)


lists = 0
for line in sys.stdin:
    message = json.loads(line)
    log.write(json.dumps(message) + "\n")
    log.flush()
    if "id" not in message:
        continue
    method = message["method"]
    fault = scenario.get("faults", {}).get(method)
    if fault == "hang":
        time.sleep(3600)
    if fault == "exit":
        sys.exit(3)
    if fault == "garbage":
        print("{not json", flush=True)
        continue
    if fault == "request":
        reply({"id": 900, "method": "item/tool/requestUserInput", "params": {}})
        continue
    reply({"method": "remoteControl/status/changed", "params": {}})
    if fault == "error":
        reply({"id": message["id"], "error": {"code": -32603, "message": "synthetic"}})
        continue
    params = message.get("params", {})
    if method == "initialize":
        result = {"codexHome": os.environ.get("CODEX_HOME", home + "/.codex"), "platformFamily": "unix", "platformOs": "linux", "userAgent": "fake"}
    elif method == "config/read":
        user = {"name": {"type": "user", "file": home + "/.codex/config.toml", "profile": None}, "version": version(), "config": state["config"], "disabledReason": None}
        result = {"config": {}, "origins": {}, "layers": [user] + scenario.get("layers", [])}
        if scenario.get("concurrent"):
            state["version"] += 1
    elif method == "config/batchWrite":
        if params.get("expectedVersion") not in (None, version()):
            reply({"id": message["id"], "error": {"code": -32600, "message": "ConfigVersionConflict"}})
            continue
        for edit in params["edits"]:
            apply(edit)
        state["version"] += 1
        with open(state_path, "w") as sink:
            json.dump(state, sink)
        result = {"status": scenario.get("status", "ok"), "version": version(), "filePath": home + "/.codex/config.toml"}
    elif method == "hooks/list":
        lists += 1
        result = listing(params["cwds"][0], lists)
    else:
        reply({"id": message["id"], "error": {"code": -32601, "message": "method not found"}})
        continue
    reply({"id": message["id"], "result": result})
`

type grantHome struct {
	*hookHome
	project  string
	bin      string
	scenario map[string]any
	env      []string
}

type grantLog struct {
	envs     [][]string
	methods  []string
	writes   []map[string]any
	sessions int
}

func newGrantHome(t *testing.T) *grantHome {
	t.Helper()
	h := &grantHome{hookHome: newProbeHome(t), scenario: map[string]any{}}
	for _, event := range probeNative {
		delete(h.hooks, event)
	}
	for _, event := range probeCaptains {
		h.hooks[event] = []any{captainGroup(h.root, event)}
	}
	h.project = filepath.Join(h.home, "workspaces", "mono\"repo")
	if err := os.MkdirAll(h.project, 0o755); err != nil {
		t.Fatal(err)
	}
	h.config = "model = \"synthetic\"\n[projects.\"/elsewhere\"]\ntrust_level = \"untrusted\"\n[hooks.state.\"unrelated\"]\nenabled = false\n"
	h.bin = filepath.Join(h.home, ".local", "bin")
	h.write(t, ".local/bin/codex", []byte(fakeAppServer), 0o755)
	h.env = []string{"HOME=" + h.home, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=" + leaked, "CODEX_API_KEY=" + leaked, "ANTHROPIC_API_KEY=" + leaked}
	return h
}

func (h *grantHome) run(t *testing.T, mode orca.GrantMode, seconds string) (orca.HookGrantResult, error) {
	t.Helper()
	h.stage(t)
	h.write(t, ".local/bin/scenario.json", h.document(t, h.scenario), 0o600)
	cmd := exec.Command("python3", append([]string{"-c", orca.HookGrantScript, string(mode), probeCaptain, probeVersion, "0.159.2", h.project, seconds}, orca.CredentialEnv()...)...)
	cmd.Env = h.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the grant helper exited unsuccessfully: %v", err)
	}
	for _, private := range []string{leaked, "CAPT_HOOK_PROVIDER", "codex-hook.sh", "synthetic"} {
		if strings.Contains(string(out), private) {
			t.Errorf("the grant output carries %q: %s", private, out)
		}
	}
	result, perr := orca.ParseHookGrant(out, orca.Pin{ID: probeCaptain, Version: probeVersion}, mode, h.project)
	if perr != nil && strings.Contains(perr.Error(), leaked) {
		t.Errorf("the grant diagnostic carries a credential: %v", perr)
	}
	return result, perr
}

func (h *grantHome) log(t *testing.T) grantLog {
	t.Helper()
	var log grantLog
	path := filepath.Join(h.bin, "requests.jsonl")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return log
	}
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var line struct {
			Env    []string       `json:"env"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Env != nil {
			log.envs, log.sessions = append(log.envs, line.Env), log.sessions+1
			continue
		}
		log.methods = append(log.methods, line.Method)
		if line.Method == "config/batchWrite" {
			log.writes = append(log.writes, line.Params)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return log
}

func (h *grantHome) saved(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.bin, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state.Config
}

func (h *grantHome) moveForOrca(t *testing.T, rebase bool) {
	t.Helper()
	config := h.saved(t)
	states := config["hooks"].(map[string]any)["state"].(map[string]any)
	source := h.home + "/.codex/hooks.json"
	for _, event := range probeNative {
		h.hooks[event] = append([]any{nativeGroup(h.home)}, h.hooks[event]...)
	}
	listed := map[string]string{}
	for key, value := range states {
		if hash, ok := value.(map[string]any)["trusted_hash"].(string); ok {
			listed[key] = hash
		}
	}
	for key, hash := range listed {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(key, source+":"), ":")
		if rebase {
			delete(states, key)
			states[source+":"+prefix+":1:0"] = map[string]any{"trusted_hash": hash}
		}
	}
	h.write(t, ".local/bin/state.json", h.document(t, map[string]any{"config": config, "version": 100}), 0o600)
}

func TestHookGrantFillsTheFiveThroughTheNativeProtocol(t *testing.T) {
	h := newGrantHome(t)
	h.env = append(h.env, "CODEX_HOME="+h.home+"/.codex")
	result, err := h.run(t, orca.GrantFill, "20")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ProjectWrite || result.HookWrites != 5 || len(result.Hooks) != 5 || result.Project != h.project {
		t.Fatalf("result = %+v", result)
	}
	log := h.log(t)
	if want := []string{"initialize", "initialized", "config/read", "config/batchWrite", "hooks/list", "config/batchWrite", "hooks/list"}; !slices.Equal(log.methods, want) {
		t.Fatalf("methods = %q, want %q", log.methods, want)
	}
	for _, env := range log.envs {
		for _, name := range orca.CredentialEnv() {
			if slices.Contains(env, name) {
				t.Errorf("the app-server inherited %s", name)
			}
		}
		if !slices.Contains(env, "CODEX_HOME") {
			t.Error("the bound configuration target was stripped")
		}
	}
	project := log.writes[0]
	wantKey := `projects."` + strings.ReplaceAll(h.project, `"`, `\"`) + `".trust_level`
	edits := project["edits"].([]any)
	if len(edits) != 1 || edits[0].(map[string]any)["keyPath"] != wantKey || edits[0].(map[string]any)["value"] != "trusted" || edits[0].(map[string]any)["mergeStrategy"] != "replace" || project["expectedVersion"] != "sha256:"+strings.Repeat("0", 63)+"1" {
		t.Errorf("project write = %v", project)
	}
	grant := log.writes[1]["edits"].([]any)
	upsert := grant[0].(map[string]any)
	value := upsert["value"].(map[string]any)
	if len(grant) != 1 || upsert["keyPath"] != "hooks.state" || upsert["mergeStrategy"] != "upsert" || len(value) != 5 || log.writes[1]["expectedVersion"] != "sha256:"+strings.Repeat("0", 63)+"2" {
		t.Errorf("hook grant = %v", log.writes[1])
	}
	for _, hook := range result.Hooks {
		if value[hook.Key].(map[string]any)["trusted_hash"] != hook.Hash || len(value[hook.Key].(map[string]any)) != 1 {
			t.Errorf("hook %s was granted %v, not its native hash %s", hook.Key, value[hook.Key], hook.Hash)
		}
	}
	config := h.saved(t)
	projects := config["projects"].(map[string]any)
	states := config["hooks"].(map[string]any)["state"].(map[string]any)
	if config["model"] != "synthetic" || projects["/elsewhere"].(map[string]any)["trust_level"] != "untrusted" || projects[h.project].(map[string]any)["trust_level"] != "trusted" || states["unrelated"].(map[string]any)["enabled"] != false || len(states) != 6 {
		t.Errorf("config after the grant = %v", config)
	}

	again, err := h.run(t, orca.GrantFill, "20")
	if err != nil || again.ProjectWrite || again.HookWrites != 0 || !slices.Equal(again.Hooks, result.Hooks) {
		t.Fatalf("a trusted fill = %+v, %v", again, err)
	}
	if got := h.log(t).methods; !slices.Equal(got, []string{"initialize", "initialized", "config/read", "hooks/list"}) {
		t.Errorf("a trusted fill called %q", got)
	}
	claim, err := h.run(t, orca.GrantClaim, "20")
	if err != nil || !slices.Equal(claim.Hooks, result.Hooks) || len(h.log(t).writes) != 0 {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
}

func TestHookGrantFinalSeesTheFiveSurviveOrcasPositionalMove(t *testing.T) {
	for _, rebase := range []bool{true, false} {
		h := newGrantHome(t)
		fill, err := h.run(t, orca.GrantFill, "20")
		if err != nil {
			t.Fatal(err)
		}
		h.log(t)
		h.moveForOrca(t, rebase)
		h.stage(t)
		trustOrcasOwn(t, h)
		final, err := h.run(t, orca.GrantFinal, "20")
		log := h.log(t)
		if len(log.writes) != 0 {
			t.Errorf("the final verification wrote %v", log.writes)
		}
		if !rebase {
			var refusal orca.GrantRefusal
			if !errors.As(err, &refusal) || refusal.Reason != "untrusted-hook" {
				t.Errorf("a move without native rebase = %v", err)
			}
			continue
		}
		if err != nil || len(final.Hooks) != 13 {
			t.Fatalf("final = %+v, %v", final, err)
		}
		grant := orca.HookGrant{Captain: []orca.Pin{{ID: probeCaptain, Version: probeVersion}}, Codex: "0.159.2", Project: h.project, Exec: func(_ context.Context, argv []string) ([]byte, error) {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = h.env
			return cmd.Output()
		}}
		if err := grant.Final(context.Background(), orca.Pregrant{Project: h.project, Hooks: fill.Hooks}); err != nil {
			t.Errorf("the five did not survive the move: %v", err)
		}
		moved := 0
		for _, hook := range final.Hooks {
			if slices.ContainsFunc(fill.Hooks, func(prior orca.GrantedHook) bool { return prior.Hash == hook.Hash && prior.Key != hook.Key }) {
				moved++
			}
		}
		if moved != 5 {
			t.Errorf("%d of the five moved keys", moved)
		}
	}
}

func trustOrcasOwn(t *testing.T, h *grantHome) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.bin, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	h.scenario = map[string]any{}
	h.write(t, ".local/bin/scenario.json", h.document(t, h.scenario), 0o600)
	cmd := exec.Command(filepath.Join(h.bin, "codex"), "app-server")
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(`{"id":1,"method":"hooks/list","params":{"cwds":["` + strings.ReplaceAll(h.project, `"`, `\"`) + `"]}}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result struct {
			Data []struct {
				Hooks []struct {
					Key         string `json:"key"`
					CurrentHash string `json:"currentHash"`
					Command     string `json:"command"`
				} `json:"hooks"`
			} `json:"data"`
		} `json:"result"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &reply); err != nil {
		t.Fatal(err)
	}
	states := state["config"].(map[string]any)["hooks"].(map[string]any)["state"].(map[string]any)
	for _, hook := range reply.Result.Data[0].Hooks {
		if strings.Contains(hook.Command, "codex-hook.sh") {
			states[hook.Key] = map[string]any{"trusted_hash": hook.CurrentHash}
		}
	}
	h.write(t, ".local/bin/state.json", h.document(t, state), 0o600)
	h.log(t)
}

func TestHookGrantRefusesWithoutAnUnsafeWrite(t *testing.T) {
	tomlKey := func(h *grantHome) string { return `"` + strings.ReplaceAll(h.project, `"`, `\"`) + `"` }
	changed := "sha256:" + strings.Repeat("f", 64)
	tests := []struct {
		name    string
		mode    orca.GrantMode
		seconds string
		edit    func(*grantHome)
		want    orca.GrantRefusal
		writes  int
	}{
		{"explicit untrusted project", orca.GrantFill, "", func(h *grantHome) {
			h.config += "[projects." + tomlKey(h) + "]\ntrust_level = \"untrusted\"\n"
		}, orca.GrantRefusal{Reason: "project-untrusted", Record: -1}, 0},
		{"untrusted by another layer", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["layers"] = []any{map[string]any{"name": map[string]any{"type": "system", "file": "/etc/codex/config.toml"}, "version": "sha256:" + strings.Repeat("e", 64), "disabledReason": nil, "config": map[string]any{"projects": map[string]any{h.project: map[string]any{"trust_level": "untrusted"}}}}}
		}, orca.GrantRefusal{Reason: "project-untrusted", Record: -1}, 0},
		{"another codex", orca.GrantFill, "", func(h *grantHome) { h.scenario["version"] = "codex-cli 0.159.1" }, orca.GrantRefusal{Reason: "codex", Record: -1}, 0},
		{"symlinked project", orca.GrantFill, "", func(h *grantHome) {
			link := filepath.Join(h.home, "alias")
			if err := os.Symlink(h.project, link); err != nil {
				t.Fatal(err)
			}
			h.project = link
		}, orca.GrantRefusal{Reason: "project", Record: -1}, 0},
		{"inline handler", orca.GrantFill, "", func(h *grantHome) { h.config += "[[hooks.PreToolUse]]\ncommand = \"x\"\n" }, orca.GrantRefusal{Reason: "inline-hooks", Record: -1}, 0},
		{"orca handlers before the fill", orca.GrantFill, "", func(h *grantHome) {
			for _, event := range probeNative {
				h.hooks[event] = append([]any{nativeGroup(h.home)}, h.hooks[event]...)
			}
		}, orca.GrantRefusal{Reason: "unexpected-event", Record: -1}, 0},
		{"extra project hook", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["extra"] = []any{map[string]any{"key": h.project + "/.codex/hooks.json:stop:0:0", "currentHash": changed, "trustStatus": "untrusted", "enabled": true, "source": "project", "sourcePath": h.project + "/.codex/hooks.json", "isManaged": false, "pluginId": nil, "eventName": "stop", "matcher": nil, "statusMessage": nil, "timeoutSec": 600, "handlerType": "command", "command": "x", "async": false, "displayOrder": 9, "additionalContextLimit": nil}}
		}, orca.GrantRefusal{Reason: "extra", Event: "Stop", Record: 5}, 1},
		{"matcher drift", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["patch"] = map[string]any{"postToolUse": map[string]any{"matcher": "*"}}
		}, orca.GrantRefusal{Reason: "extra", Event: "PostToolUse"}, 1},
		{"timeout drift", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["patch"] = map[string]any{"sessionStart": map[string]any{"timeoutSec": 30}}
		}, orca.GrantRefusal{Reason: "extra", Event: "SessionStart"}, 1},
		{"disabled handler", orca.GrantFill, "", func(h *grantHome) { h.scenario["patch"] = map[string]any{"stop": map[string]any{"enabled": false}} }, orca.GrantRefusal{Reason: "disabled", Event: "Stop"}, 1},
		{"modified handler", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["patch"] = map[string]any{"preToolUse": map[string]any{"trustStatus": "modified"}}
		}, orca.GrantRefusal{Reason: "modified", Event: "PreToolUse"}, 1},
		{"duplicate key", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["patch"] = map[string]any{"stop": map[string]any{"key": "same"}, "sessionStart": map[string]any{"key": "same"}}
		}, orca.GrantRefusal{Reason: "duplicate", Event: "Stop"}, 1},
		{"missing listing", orca.GrantFill, "", func(h *grantHome) { h.scenario["drop"] = []any{"userPromptSubmit"} }, orca.GrantRefusal{Reason: "missing", Event: "UserPromptSubmit", Record: -1}, 1},
		{"hash changed between lists", orca.GrantFill, "", func(h *grantHome) {
			h.scenario["patchCall"] = map[string]any{"2": map[string]any{"stop": map[string]any{"currentHash": changed}}}
		}, orca.GrantRefusal{Reason: "changed", Record: -1}, 2},
		{"rpc error", orca.GrantFill, "", func(h *grantHome) { h.scenario["faults"] = map[string]any{"hooks/list": "error"} }, orca.GrantRefusal{Reason: "rpc", Record: -1, Step: "hooks/list"}, 1},
		{"malformed response", orca.GrantFill, "", func(h *grantHome) { h.scenario["faults"] = map[string]any{"config/read": "garbage"} }, orca.GrantRefusal{Reason: "protocol", Record: -1, Step: "config/read"}, 0},
		{"server request", orca.GrantFill, "", func(h *grantHome) { h.scenario["faults"] = map[string]any{"config/read": "request"} }, orca.GrantRefusal{Reason: "protocol", Record: -1, Step: "config/read"}, 0},
		{"truncated session", orca.GrantFill, "", func(h *grantHome) { h.scenario["faults"] = map[string]any{"hooks/list": "exit"} }, orca.GrantRefusal{Reason: "exit", Record: -1, Step: "hooks/list"}, 1},
		{"deadline", orca.GrantFill, "1", func(h *grantHome) { h.scenario["faults"] = map[string]any{"hooks/list": "hang"} }, orca.GrantRefusal{Reason: "timeout", Record: -1, Step: "hooks/list"}, 1},
		{"concurrent config change", orca.GrantFill, "", func(h *grantHome) { h.scenario["concurrent"] = true }, orca.GrantRefusal{Reason: "rpc", Record: -1, Step: "config/batchWrite"}, 1},
		{"overridden write", orca.GrantFill, "", func(h *grantHome) { h.scenario["status"] = "okOverridden" }, orca.GrantRefusal{Reason: "overridden", Record: -1, Step: "config/batchWrite"}, 1},
		{"claim of an ungranted project", orca.GrantClaim, "", func(*grantHome) {}, orca.GrantRefusal{Reason: "project-untrusted", Record: -1}, 0},
		{"claim of ungranted hooks", orca.GrantClaim, "", func(h *grantHome) {
			h.config += "[projects." + tomlKey(h) + "]\ntrust_level = \"trusted\"\n"
		}, orca.GrantRefusal{Reason: "untrusted-hook", Event: "PostToolUse"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGrantHome(t)
			tt.edit(h)
			_, err := h.run(t, tt.mode, cmp.Or(tt.seconds, "20"))
			var refusal orca.GrantRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("grant = %v, want a refusal", err)
			}
			want := tt.want
			want.Mode = tt.mode
			if want.Event != "" && want.Record == 0 {
				want.Record = refusal.Record
			}
			if refusal != want {
				t.Errorf("refusal = %+v, want %+v", refusal, want)
			}
			log := h.log(t)
			if len(log.writes) != tt.writes {
				t.Errorf("%d writes, want %d: %v", len(log.writes), tt.writes, log.writes)
			}
			for _, write := range log.writes {
				for _, edit := range write["edits"].([]any) {
					if edit.(map[string]any)["keyPath"] == "hooks.state" && want.Reason != "changed" {
						t.Errorf("a refused grant wrote hook trust: %v", edit)
					}
				}
			}
		})
	}
}

func TestHookGrantRefusesAnotherCodexHomeBeforeAnyChild(t *testing.T) {
	for _, selected := range []string{"/foreign/.codex", "/.codex/", "/.codex/../.codex"} {
		t.Run(selected, func(t *testing.T) {
			h := newGrantHome(t)
			foreign := h.home + selected
			h.env = append(h.env, "CODEX_HOME="+foreign)
			_, err := h.run(t, orca.GrantFill, "20")
			var refusal orca.GrantRefusal
			if !errors.As(err, &refusal) || refusal != (orca.GrantRefusal{Mode: orca.GrantFill, Reason: "home", Record: -1}) {
				t.Fatalf("grant = %v, want a home refusal before any child", err)
			}
			if _, err := os.Stat(filepath.Join(h.bin, "invocations")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a Codex child ran under another configuration home: %v", err)
			}
			if _, err := os.Stat(h.home + "/foreign"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the foreign configuration home was touched: %v", err)
			}
			if entries, err := os.ReadDir(h.home + "/.codex"); err != nil || len(entries) != 2 {
				t.Errorf("the configuration home gained state: %v, %v", entries, err)
			}
		})
	}
}
