package orca_test

import (
	"crypto/sha256"
	"encoding/hex"
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

const (
	probeCaptain = "captain-hook@captain-hook"
	probeVersion = "12.79.15"
)

var (
	probeNative   = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "SubagentStart", "SubagentStop", "Stop"}
	probeCaptains = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}
)

type hookHome struct {
	home       string
	root       string
	hooks      map[string][]any
	rawHooks   string
	installed  map[string]any
	manifest   map[string]any
	config     string
	executable bool
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func nativeGroup(home string) map[string]any {
	script := shellQuote(home + "/.orca/agent-hooks/codex-hook.sh")
	command := "if [ -f " + script + " ] && [ -r " + script + " ] && [ -x " + script + " ]; then /bin/sh " + script + "; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi"
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}
}

func captainGroup(root, event string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "CAPT_HOOK_PROVIDER=codex " + shellQuote(root+"/bin/hook") + " run " + event}}}
}

func newProbeHome(t *testing.T) *hookHome {
	t.Helper()
	home := t.TempDir()
	root := home + "/.claude/plugins/cache/captain-hook/captain-hook/" + probeVersion
	h := &hookHome{
		home:       home,
		root:       root,
		hooks:      map[string][]any{},
		installed:  map[string]any{"version": 2, "plugins": map[string]any{probeCaptain: []any{map[string]any{"scope": "user", "installPath": root, "version": probeVersion, "gitCommitSha": leaked}}}},
		manifest:   map[string]any{"name": "captain-hook", "version": probeVersion, "description": leaked},
		config:     "model = \"synthetic\"\n[hooks]\nenabled = true\n[hooks.state]\n\"synthetic-entry\" = \"" + leaked + "\"\n",
		executable: true,
	}
	for _, event := range probeNative {
		h.hooks[event] = []any{nativeGroup(home)}
		if slices.Contains(probeCaptains, event) {
			h.hooks[event] = append(h.hooks[event], captainGroup(root, event))
		}
	}
	return h
}

func (h *hookHome) write(t *testing.T, rel string, data []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(h.home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func (h *hookHome) document(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (h *hookHome) run(t *testing.T) (string, []byte, orca.HookProbe, error) {
	t.Helper()
	hooks := []byte(h.rawHooks)
	if h.rawHooks == "" {
		hooks = h.document(t, map[string]any{"hooks": h.hooks})
	}
	h.write(t, ".codex/hooks.json", hooks, 0o600)
	h.write(t, ".codex/config.toml", []byte(h.config), 0o600)
	h.write(t, ".claude/plugins/installed_plugins.json", h.document(t, h.installed), 0o600)
	rel := strings.TrimPrefix(h.root, h.home+"/")
	h.write(t, rel+"/.claude-plugin/plugin.json", h.document(t, h.manifest), 0o600)
	mode := os.FileMode(0o644)
	if h.executable {
		mode = 0o755
	}
	h.write(t, rel+"/bin/hook", []byte("#!/bin/sh\n"), mode)
	cmd := exec.Command("python3", "-c", orca.HookProbeScript, probeCaptain, probeVersion)
	cmd.Env = []string{"HOME=" + h.home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the probe exited unsuccessfully: %v", err)
	}
	for _, private := range []string{leaked, "CAPT_HOOK_PROVIDER", "codex-hook.sh", "synthetic-entry"} {
		if strings.Contains(string(out), private) {
			t.Errorf("the probe output carries %q: %s", private, out)
		}
	}
	probe, perr := orca.ParseHookProbe(out, orca.Pin{ID: probeCaptain, Version: probeVersion})
	if perr != nil && strings.Contains(perr.Error(), leaked) {
		t.Errorf("the probe diagnostic carries a credential: %v", perr)
	}
	return string(out), hooks, probe, perr
}

func TestHookProbeComparesTheThirteenDefinitionsPrivately(t *testing.T) {
	h := newProbeHome(t)
	_, hooks, probe, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(hooks)
	if probe.Home != h.home || probe.Root != h.root || probe.Definitions != 13 || probe.HooksSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("probe = %+v", probe)
	}
	for _, event := range probeNative {
		want := 1
		if slices.Contains(probeCaptains, event) {
			want = 2
		}
		if probe.Events[event] != want {
			t.Errorf("%s multiplicity %d, want %d", event, probe.Events[event], want)
		}
	}
	reversed := newProbeHome(t)
	for event, groups := range reversed.hooks {
		slices.Reverse(groups)
		reversed.hooks[event] = groups
	}
	if _, _, _, err := reversed.run(t); err != nil {
		t.Errorf("the installer order of the same definitions was refused: %v", err)
	}
}

func TestHookProbeRefusesChangedDefinitionsAndInputs(t *testing.T) {
	group := func(h *hookHome, event string, index int) map[string]any {
		return h.hooks[event][index].(map[string]any)
	}
	handler := func(h *hookHome, event string, index int) map[string]any {
		return group(h, event, index)["hooks"].([]any)[0].(map[string]any)
	}
	tests := []struct {
		name string
		edit func(*hookHome)
		want orca.HookRefusal
	}{
		{"extra definition", func(h *hookHome) { h.hooks["Stop"] = append(h.hooks["Stop"], captainGroup(h.root, "Stop")) }, orca.HookRefusal{Reason: "mismatch", Event: "Stop", Record: 2}},
		{"missing generic definition", func(h *hookHome) { h.hooks["PreToolUse"] = h.hooks["PreToolUse"][:1] }, orca.HookRefusal{Reason: "missing", Event: "PreToolUse", Record: 1}},
		{"missing native event", func(h *hookHome) { delete(h.hooks, "SubagentStart") }, orca.HookRefusal{Reason: "missing", Event: "SubagentStart", Record: 0}},
		{"duplicate generic definition", func(h *hookHome) {
			h.hooks["SessionStart"] = []any{nativeGroup(h.home), captainGroup(h.root, "SessionStart"), captainGroup(h.root, "SessionStart")}
		}, orca.HookRefusal{Reason: "mismatch", Event: "SessionStart", Record: 2}},
		{"observer wrapped", func(h *hookHome) {
			handler(h, "Stop", 1)["command"] = "observer " + handler(h, "Stop", 1)["command"].(string)
		}, orca.HookRefusal{Reason: "mismatch", Event: "Stop", Record: 1}},
		{"generic matcher", func(h *hookHome) { group(h, "SessionStart", 1)["matcher"] = "*" }, orca.HookRefusal{Reason: "mismatch", Event: "SessionStart", Record: 1}},
		{"generic async", func(h *hookHome) { handler(h, "PostToolUse", 1)["async"] = true }, orca.HookRefusal{Reason: "mismatch", Event: "PostToolUse", Record: 1}},
		{"generic timeout", func(h *hookHome) { handler(h, "UserPromptSubmit", 1)["timeout"] = 10 }, orca.HookRefusal{Reason: "mismatch", Event: "UserPromptSubmit", Record: 1}},
		{"native timeout changed", func(h *hookHome) { handler(h, "PermissionRequest", 0)["timeout"] = 5 }, orca.HookRefusal{Reason: "mismatch", Event: "PermissionRequest", Record: 0}},
		{"native timeout as a float", func(h *hookHome) { handler(h, "SubagentStop", 0)["timeout"] = 10.5 }, orca.HookRefusal{Reason: "mismatch", Event: "SubagentStop", Record: 0}},
		{"credential command", func(h *hookHome) {
			handler(h, "UserPromptSubmit", 1)["command"] = "OPENAI_API_KEY=" + leaked + " hook"
		}, orca.HookRefusal{Reason: "mismatch", Event: "UserPromptSubmit", Record: 1}},
		{"another runtime home", func(h *hookHome) {
			for _, event := range probeNative {
				h.hooks[event][0] = nativeGroup("/home/sprite")
			}
		}, orca.HookRefusal{Reason: "mismatch", Event: "SessionStart", Record: 0}},
		{"unexpected event", func(h *hookHome) { h.hooks["Notification"+leaked] = []any{nativeGroup(h.home)} }, orca.HookRefusal{Reason: "unexpected-event", Record: -1}},
		{"unknown document key", func(h *hookHome) {
			raw, err := json.Marshal(map[string]any{"hooks": h.hooks, leaked: true})
			if err != nil {
				panic(err)
			}
			h.rawHooks = string(raw)
		}, orca.HookRefusal{Reason: "hooks", Record: -1}},
		{"duplicate key", func(h *hookHome) {
			h.rawHooks = `{"hooks":{"Stop":[],"Stop":[]}}`
		}, orca.HookRefusal{Reason: "duplicate-key", Record: -1}},
		{"inline handler beside native bookkeeping", func(h *hookHome) {
			h.config += "[[hooks.PreToolUse]]\ncommand = \"" + leaked + "\"\n"
		}, orca.HookRefusal{Reason: "inline-hooks", Record: -1}},
		{"nested inline handler", func(h *hookHome) {
			h.config += "[profiles.fast.hooks]\nStop = \"" + leaked + "\"\n"
		}, orca.HookRefusal{Reason: "inline-hooks", Record: -1}},
		{"unreadable config", func(h *hookHome) { h.config = "[hooks\n" }, orca.HookRefusal{Reason: "config", Record: -1}},
		{"installed version", func(h *hookHome) {
			entry := h.installed["plugins"].(map[string]any)[probeCaptain].([]any)[0].(map[string]any)
			entry["version"] = "12.79.8"
		}, orca.HookRefusal{Reason: "metadata", Record: -1}},
		{"install path", func(h *hookHome) {
			entry := h.installed["plugins"].(map[string]any)[probeCaptain].([]any)[0].(map[string]any)
			entry["installPath"] = h.home + "/elsewhere"
		}, orca.HookRefusal{Reason: "metadata", Record: -1}},
		{"project scope", func(h *hookHome) {
			entry := h.installed["plugins"].(map[string]any)[probeCaptain].([]any)[0].(map[string]any)
			entry["scope"] = "project"
		}, orca.HookRefusal{Reason: "metadata", Record: -1}},
		{"manifest version", func(h *hookHome) { h.manifest["version"] = "12.79.8" }, orca.HookRefusal{Reason: "manifest", Record: -1}},
		{"manifest name", func(h *hookHome) { h.manifest["name"] = "other" }, orca.HookRefusal{Reason: "manifest", Record: -1}},
		{"hook not executable", func(h *hookHome) { h.executable = false }, orca.HookRefusal{Reason: "executable", Record: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProbeHome(t)
			tt.edit(h)
			out, _, _, err := h.run(t)
			var refusal orca.HookRefusal
			if !errors.As(err, &refusal) || refusal != tt.want {
				t.Fatalf("probe = %s, %v; want %+v", out, err, tt.want)
			}
		})
	}
}
