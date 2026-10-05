package orca_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const fakeConfigServer = `#!/usr/bin/env python3
import json
import os
import sys

here = os.path.dirname(os.path.realpath(__file__))
with open(os.path.join(here, "scenario.json")) as source:
    scenario = json.load(source)
if sys.argv[1:] == ["--version"]:
    print(scenario.get("version", "codex-cli 0.159.2"))
    sys.exit(0)
log = open(os.path.join(here, "requests.jsonl"), "a")
log.write(json.dumps({"argv": sys.argv[1:], "env": sorted(os.environ)}) + "\n")
log.flush()
for line in sys.stdin:
    message = json.loads(line)
    log.write(json.dumps(message) + "\n")
    log.flush()
    if "id" not in message:
        continue
    if message["method"] == "initialize":
        result = {"codexHome": scenario.get("codexHome", os.environ["HOME"] + "/.codex")}
    elif message["method"] == "config/read":
        result = {"config": scenario.get("config", {}), "origins": {}, "layers": scenario.get("layers", [])}
    else:
        print(json.dumps({"id": message["id"], "error": {"code": -32601, "message": "method not found"}}), flush=True)
        continue
    print(json.dumps({"id": message["id"], "result": result}), flush=True)
`

type policyHome struct {
	home     string
	project  string
	bin      string
	scenario map[string]any
	env      []string
}

func newPolicyHome(t *testing.T) *policyHome {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &policyHome{home: home, project: filepath.Join(home, "workspaces", "repo"), bin: filepath.Join(home, ".local", "bin"), scenario: map[string]any{}}
	for _, dir := range []string{h.project, h.bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.bin, "codex"), []byte(fakeConfigServer), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	for _, name := range orca.CredentialEnv() {
		h.env = append(h.env, name+"="+leaked)
	}
	return h
}

func (h *policyHome) run(t *testing.T) (orca.ShellPolicy, error) {
	t.Helper()
	raw, err := json.Marshal(h.scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.bin, "scenario.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", append([]string{"-c", orca.ShellPolicyScript, "0.159.2", h.project, "30"}, orca.CredentialEnv()...)...)
	cmd.Env = h.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the policy reader exited unsuccessfully: %v", err)
	}
	if strings.Contains(string(out), leaked) || strings.Contains(string(out), "github.example") {
		t.Errorf("the policy reader printed a policy value: %s", out)
	}
	return orca.ParseShellPolicy(out)
}

func (h *policyHome) requests(t *testing.T) ([]string, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.bin, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	var messages []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if names, ok := line["env"].([]any); ok {
			for _, name := range names {
				env = append(env, name.(string))
			}
			continue
		}
		messages = append(messages, line)
	}
	return env, messages
}

func policyLayer(kind string, policy map[string]any) map[string]any {
	config := map[string]any{"model": "synthetic"}
	if policy != nil {
		config["shell_environment_policy"] = policy
	}
	return map[string]any{"name": map[string]any{"type": kind}, "version": "sha256:" + strings.Repeat("0", 64), "config": config, "disabledReason": nil}
}

func effectivePolicy(fields map[string]any) map[string]any {
	policy := map[string]any{"inherit": nil, "ignore_default_excludes": nil, "exclude": nil, "set": nil, "include_only": nil, "filters": nil, "experimental_use_profile": nil}
	for key, value := range fields {
		policy[key] = value
	}
	return map[string]any{"model": "synthetic", "shell_environment_policy": policy}
}

func TestShellPolicyScriptReadsTheEffectiveRepresentation(t *testing.T) {
	keyed := map[string]any{"inherit": "core", "set": map[string]any{"GH_HOST": "github.example"}, "filters": map[string]any{"AWS_*": "exclude", "PATH": "include"}}
	legacy := map[string]any{"exclude": []any{"AWS_*", "GITHUB_TOKEN"}, "include_only": []any{"*"}, "set": map[string]any{"GH_HOST": "github.example"}}
	tests := []struct {
		name   string
		layers []any
		config map[string]any
		want   orca.ShellPolicy
	}{
		{"no policy anywhere", []any{policyLayer("user", nil)}, effectivePolicy(nil), orca.ShellPolicy{}},
		{"keyed filters keep every other field", []any{policyLayer("project", map[string]any{"ignore_default_excludes": false}), policyLayer("user", keyed)}, effectivePolicy(map[string]any{"inherit": "core", "ignore_default_excludes": false, "set": map[string]any{"GH_HOST": "github.example"}, "filters": map[string]any{"aws_*": "exclude", "path": "include"}}), orca.ShellPolicy{}},
		{"legacy exclusions carry forward in order", []any{policyLayer("user", legacy)}, effectivePolicy(legacy), orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*", "GITHUB_TOKEN"}}},
		{"a legacy include list alone", []any{policyLayer("system", map[string]any{"include_only": []any{"PATH", "HOME"}})}, effectivePolicy(map[string]any{"include_only": []any{"PATH", "HOME"}}), orca.ShellPolicy{Legacy: true}},
		{"a protected filter left to the session flags", []any{policyLayer("user", map[string]any{"filters": map[string]any{"OPENAI_API_KEY": "include"}})}, effectivePolicy(map[string]any{"filters": map[string]any{"openai_api_key": "include"}}), orca.ShellPolicy{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPolicyHome(t)
			h.scenario["layers"], h.scenario["config"] = tt.layers, tt.config
			got, err := h.run(t)
			if err != nil || got.Legacy != tt.want.Legacy || !slices.Equal(got.Exclude, tt.want.Exclude) {
				t.Fatalf("policy = %+v, %v; want %+v", got, err, tt.want)
			}
			env, messages := h.requests(t)
			for _, name := range orca.CredentialEnv() {
				if slices.Contains(env, name) {
					t.Errorf("the config server inherited %s", name)
				}
			}
			if !slices.Contains(env, "HOME") {
				t.Errorf("the config server env lost unrelated variables: %q", env)
			}
			methods := make([]string, 0, len(messages))
			for _, message := range messages {
				methods = append(methods, message["method"].(string))
			}
			if !slices.Equal(methods, []string{"initialize", "initialized", "config/read"}) {
				t.Errorf("methods = %q", methods)
			}
			if params := messages[2]["params"]; !reflect.DeepEqual(params, map[string]any{"includeLayers": true, "cwd": h.project}) {
				t.Errorf("config/read params = %v", params)
			}
		})
	}
}

func TestShellPolicyScriptRefusesWhatTheSessionFlagsCannotOverride(t *testing.T) {
	disabled := policyLayer("project", map[string]any{"exclude": []any{"AWS_*"}})
	disabled["disabledReason"] = "untrusted project"
	tests := []struct {
		name  string
		edit  func(*policyHome)
		want  orca.ShellPolicyRefusal
		layer []any
	}{
		{"set restores a protected key", nil, orca.ShellPolicyRefusal{Reason: "set", Layer: "project", Record: 1}, []any{policyLayer("user", nil), policyLayer("project", map[string]any{"set": map[string]any{"OPENAI_API_KEY": leaked}})}},
		{"set restores a protected key in another case", nil, orca.ShellPolicyRefusal{Reason: "set", Layer: "user"}, []any{policyLayer("user", map[string]any{"set": map[string]any{"codex_api_key": leaked}})}},
		{"a managed layer outranks the session flags", nil, orca.ShellPolicyRefusal{Reason: "managed", Layer: "legacyManagedConfigTomlFromFile"}, []any{policyLayer("legacyManagedConfigTomlFromFile", map[string]any{"exclude": []any{"AWS_*"}}), policyLayer("user", nil)}},
		{"a disabled project layer joins after folder trust", nil, orca.ShellPolicyRefusal{Reason: "disabled", Layer: "project", Record: 1}, []any{policyLayer("user", nil), disabled}},
		{"another codex", func(h *policyHome) { h.scenario["version"] = "codex-cli 0.160.0" }, orca.ShellPolicyRefusal{Reason: "codex", Record: -1}, []any{policyLayer("user", nil)}},
		{"another codex home", func(h *policyHome) { h.scenario["codexHome"] = "/elsewhere/.codex" }, orca.ShellPolicyRefusal{Reason: "home", Record: -1, Step: "initialize"}, []any{policyLayer("user", nil)}},
		{"a CODEX_HOME override", func(h *policyHome) { h.env = append(h.env, "CODEX_HOME=/elsewhere/.codex") }, orca.ShellPolicyRefusal{Reason: "home", Record: -1}, []any{policyLayer("user", nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPolicyHome(t)
			h.scenario["layers"], h.scenario["config"] = tt.layer, effectivePolicy(nil)
			if tt.edit != nil {
				tt.edit(h)
			}
			_, err := h.run(t)
			var refusal orca.ShellPolicyRefusal
			if !errors.As(err, &refusal) || refusal != tt.want {
				t.Fatalf("policy err = %v, want %+v", err, tt.want)
			}
		})
	}
}

func TestShellPolicyOverrideIsTheTOMLCodexReads(t *testing.T) {
	excluded := map[string]any{}
	for _, name := range orca.CredentialEnv() {
		excluded[name] = "exclude"
	}
	tests := []struct {
		name   string
		policy orca.ShellPolicy
		want   map[string]any
	}{
		{"keyed", orca.ShellPolicy{}, map[string]any{"filters": excluded}},
		{"legacy", orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*", "[!A]?_TOKEN", `A"B\C`, "TAB\tDEL\x7f", "é<&>"}}, map[string]any{"exclude": []any{"AWS_*", "[!A]?_TOKEN", `A"B\C`, "TAB\tDEL\x7f", "é<&>", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv := orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e"}.Argv(tt.policy)
			i := slices.IndexFunc(argv, func(arg string) bool { return strings.HasPrefix(arg, "shell_environment_policy.") })
			if i < 1 || argv[i-1] != "-c" || slices.ContainsFunc(argv[i+1:], func(arg string) bool { return strings.HasPrefix(arg, "shell_environment_policy.") }) {
				t.Fatalf("argv carries no single shell policy override: %q", argv)
			}
			out, err := exec.Command("python3", "-c", "import json, sys, tomllib; print(json.dumps(tomllib.loads(sys.argv[1])))", argv[i]).Output()
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if want := map[string]any{"shell_environment_policy": tt.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("override = %v, want %v", got, want)
			}
		})
	}
}
