package images

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const fakeClaude = `#!/usr/bin/env python3
import json
import os
import sys

state_path = os.environ["FAKE_STATE"]
settings_path = os.path.join(os.environ["HOME"], ".claude", "settings.json")
catalog = json.load(open(os.environ["FAKE_CATALOG"]))
state = json.load(open(state_path))
args = sys.argv[1:]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("claude " + " ".join(args) + "\n")


def save():
    json.dump(state, open(state_path, "w"))


def settings():
    return json.load(open(settings_path)) if os.path.exists(settings_path) else {}


def save_settings(data):
    os.makedirs(os.path.dirname(settings_path), exist_ok=True)
    json.dump(data, open(settings_path, "w"))


def marketplace(name):
    return next((m for m in state["marketplaces"] if m["name"] == name), None)


def fail(message):
    print(message, file=sys.stderr)
    sys.exit(1)


if args == ["plugin", "marketplace", "list", "--json"]:
    print(json.dumps([{k: v for k, v in m.items() if k not in ("key", "snapshot")} for m in state["marketplaces"]]))
elif args[:3] == ["plugin", "marketplace", "add"]:
    source = args[3]
    if source.startswith("/"):
        key = "dir:" + os.path.basename(source)
        entry = {"source": "directory", "path": source}
    else:
        repo, _, ref = source.partition("#")
        key = "github:" + source
        entry = {"source": "github", "repo": repo}
        if ref:
            entry["ref"] = ref
    if key not in catalog:
        fail("no marketplace at " + source)
    name = catalog[key]["name"]
    if name == "claude-plugins-official" and entry["source"] != "github":
        fail("The marketplace name 'claude-plugins-official' can only be used with GitHub sources from the 'anthropics' org")
    if marketplace(name):
        fail("marketplace " + name + " is already installed")
    state["marketplaces"].append(dict(entry, name=name, key=key, snapshot=catalog[key]["plugins"]))
    save()
    declared = settings()
    declared.setdefault("extraKnownMarketplaces", {})[name] = {"source": entry}
    save_settings(declared)
elif args[:3] == ["plugin", "marketplace", "update"]:
    source = marketplace(args[3])
    if not source:
        fail("no marketplace " + args[3])
    source["snapshot"] = catalog[source["key"]]["plugins"]
    save()
elif args[:3] == ["plugin", "marketplace", "remove"]:
    if not marketplace(args[3]):
        fail("no marketplace " + args[3])
    state["marketplaces"] = [m for m in state["marketplaces"] if m["name"] != args[3]]
    state["plugins"] = [p for p in state["plugins"] if not p["id"].endswith("@" + args[3])]
    save()
    declared = settings()
    declared.get("extraKnownMarketplaces", {}).pop(args[3], None)
    save_settings(declared)
elif args == ["plugin", "list", "--json"]:
    print(json.dumps(state["plugins"]))
elif args[:2] in (["plugin", "install"], ["plugin", "update"]):
    name, _, market = args[2].partition("@")
    source = marketplace(market)
    if not source:
        fail("no marketplace " + market)
    version = source["snapshot"][name]
    state["plugins"] = [p for p in state["plugins"] if p["id"] != args[2]]
    state["plugins"].append({"id": args[2], "version": version, "enabled": True, "errors": []})
    save()
elif args == ["auto-update"]:
    declared = settings().get("extraKnownMarketplaces", {})
    for source in state["marketplaces"]:
        if declared.get(source["name"], {}).get("autoUpdate", source["name"] == "claude-plugins-official"):
            source["snapshot"] = catalog[source["key"]]["plugins"]
            for plugin in state["plugins"]:
                name, _, market = plugin["id"].partition("@")
                if market == source["name"]:
                    plugin["version"] = source["snapshot"][name]
    save()
else:
    fail("fake claude does not handle " + " ".join(args))
`

const fakeGit = `#!/bin/sh
echo "git $*" >> "$FAKE_LOG"
case "$1" in
  init) mkdir -p "$3" ;;
  -C)
    dir="$2"
    shift 2
    case "$1" in
      fetch)
        [ -n "$6" ] || exit 128
        echo "$6" > "$dir/.fake-head"
        ;;
      checkout) ;;
      rev-parse) cat "$dir/.fake-head" 2> /dev/null || exit 128 ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`

type fakeState struct {
	Marketplaces []map[string]any `json:"marketplaces"`
	Plugins      []map[string]any `json:"plugins"`
}

type pluginsHost struct {
	t     *testing.T
	home  string
	fakes string
}

func newPluginsHost(t *testing.T, marketplaces []Marketplace, catalog map[string]any, state fakeState, settings map[string]any) pluginsHost {
	h := pluginsHost{t: t, home: t.TempDir(), fakes: t.TempDir()}
	scripts, err := Render(Inventory{
		Version: SchemaVersion,
		Claude: Claude{
			Marketplaces: marketplaces,
			Plugins: []Plugin{
				{ID: "hook@tools-market", Version: "1.0.0"},
				{ID: "datadog@claude-plugins-official", Version: "0.7.17"},
			},
		},
	}, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if state.Marketplaces == nil {
		state.Marketplaces = []map[string]any{}
	}
	if state.Plugins == nil {
		state.Plugins = []map[string]any{}
	}
	for _, m := range state.Marketplaces {
		if path, ok := m["path"].(string); ok {
			m["path"] = strings.Replace(path, "HOME", h.home, 1)
		}
	}
	files := map[string][]byte{
		"claude":     []byte(fakeClaude),
		"git":        []byte(fakeGit),
		"plugins.sh": scripts.Plugins,
		"state.json": mustJSON(t, state),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(h.fakes, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.setCatalog(catalog)
	if settings != nil {
		if err := os.MkdirAll(filepath.Join(h.home, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.home, ".claude", "settings.json"), mustJSON(t, settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h pluginsHost) setCatalog(catalog map[string]any) {
	if err := os.WriteFile(filepath.Join(h.fakes, "catalog.json"), mustJSON(h.t, catalog), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h pluginsHost) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader("\n")
	cmd.Env = append(os.Environ(),
		"HOME="+h.home,
		"PATH="+h.fakes+":"+os.Getenv("PATH"),
		"FAKE_STATE="+filepath.Join(h.fakes, "state.json"),
		"FAKE_CATALOG="+filepath.Join(h.fakes, "catalog.json"),
		"FAKE_LOG="+filepath.Join(h.fakes, "calls.log"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h pluginsHost) plugins(phase string, args ...string) (string, error) {
	return h.run("bash", append([]string{filepath.Join(h.fakes, "plugins.sh"), phase}, args...)...)
}

func (h pluginsHost) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(h.fakes, "calls.log"))
	return strings.Split(strings.ReplaceAll(string(raw), h.home, "HOME"), "\n")
}

func (h pluginsHost) declared(name string) any {
	raw, err := os.ReadFile(filepath.Join(h.home, ".claude", "settings.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var settings struct {
		ExtraKnownMarketplaces map[string]any `json:"extraKnownMarketplaces"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		h.t.Fatal(err)
	}
	return settings.ExtraKnownMarketplaces[name]
}

func (h pluginsHost) version(id string) string {
	raw, err := os.ReadFile(filepath.Join(h.fakes, "state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var state fakeState
	if err := json.Unmarshal(raw, &state); err != nil {
		h.t.Fatal(err)
	}
	for _, p := range state.Plugins {
		if p["id"] == id {
			return p["version"].(string)
		}
	}
	return ""
}

var (
	officialBranch = Marketplace{Name: "claude-plugins-official", GitHub: "anthropics/claude-plugins-official", Branch: "main"}
	toolsRef       = Marketplace{Name: "tools-market", GitHub: "owner/tools-market", Ref: commit}
	heldOfficial   = map[string]any{
		"source":     map[string]any{"source": "github", "repo": "anthropics/claude-plugins-official", "ref": "main"},
		"autoUpdate": false,
	}
)

func marketplaceCatalog(official string) map[string]any {
	return map[string]any{
		"dir:tools-market": map[string]any{"name": "tools-market", "plugins": map[string]string{"hook": "1.0.0"}},
		"github:anthropics/claude-plugins-official#main": map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": official}},
		"github:anthropics/claude-plugins-official":      map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": official}},
	}
}

func healthyPlugin(id, version string) map[string]any {
	return map[string]any{"id": id, "version": version, "enabled": true, "errors": []any{}}
}

func registered(officialRef any, officialVersion string) fakeState {
	official := map[string]any{"name": "claude-plugins-official", "source": "github", "repo": "anthropics/claude-plugins-official", "key": "github:anthropics/claude-plugins-official", "snapshot": map[string]string{"datadog": officialVersion}}
	if officialRef != nil {
		official["ref"] = officialRef
		official["key"] = "github:anthropics/claude-plugins-official#main"
	}
	return fakeState{
		Marketplaces: []map[string]any{
			{"name": "tools-market", "source": "directory", "path": "HOME/.local/share/cc-remote/marketplaces/tools-market", "key": "dir:tools-market", "snapshot": map[string]string{"hook": "1.0.0"}},
			official,
		},
		Plugins: []map[string]any{healthyPlugin("hook@tools-market", "1.0.0"), healthyPlugin("datadog@claude-plugins-official", officialVersion)},
	}
}

func TestPluginsRegisterMarketplacesByRefAndBranch(t *testing.T) {
	declaredOfficial := map[string]any{"source": map[string]any{"source": "github", "repo": "anthropics/claude-plugins-official", "ref": "main"}}
	tests := []struct {
		name         string
		marketplaces []Marketplace
		catalog      func(map[string]any)
		state        fakeState
		settings     map[string]any
		wantErr      string
		wantCalls    []string
		absentCalls  []string
	}{
		{
			name:         "fresh host",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			wantCalls: []string{
				"claude plugin marketplace add HOME/.local/share/cc-remote/marketplaces/tools-market",
				"claude plugin marketplace add anthropics/claude-plugins-official#main",
				"claude plugin install datadog@claude-plugins-official",
			},
			absentCalls: []string{"git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "official added without a ref",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered(nil, "0.7.17"),
			wantCalls: []string{
				"claude plugin marketplace remove claude-plugins-official",
				"claude plugin marketplace add anthropics/claude-plugins-official#main",
				"claude plugin install datadog@claude-plugins-official",
			},
		},
		{
			name:         "official already pinned",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered("main", "0.7.17"),
			settings:     map[string]any{"extraKnownMarketplaces": map[string]any{"claude-plugins-official": declaredOfficial}},
			absentCalls:  []string{"claude plugin marketplace add", "claude plugin marketplace remove", "claude plugin install", "claude plugin update", "git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "pinned official with auto-update turned on",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered("main", "0.7.17"),
			settings:     map[string]any{"extraKnownMarketplaces": map[string]any{"claude-plugins-official": map[string]any{"source": declaredOfficial["source"], "autoUpdate": true}}},
			absentCalls:  []string{"claude plugin marketplace add", "claude plugin marketplace remove"},
		},
		{
			name:         "pinned official refreshed by update",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered("main", "0.7.16"),
			wantCalls: []string{
				"claude plugin marketplace update claude-plugins-official",
				"claude plugin update datadog@claude-plugins-official",
			},
			absentCalls: []string{"claude plugin marketplace remove", "git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "official from a local checkout",
			marketplaces: []Marketplace{toolsRef, {Name: "claude-plugins-official", GitHub: "anthropics/claude-plugins-official", Ref: commit}},
			catalog: func(c map[string]any) {
				c["dir:claude-plugins-official"] = map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.17"}}
			},
			wantErr: "can only be used with GitHub sources from the 'anthropics' org",
		},
		{
			name:         "upstream bump",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			catalog: func(c map[string]any) {
				c["github:anthropics/claude-plugins-official#main"] = map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.18"}}
			},
			wantErr: "the installed, enabled and loadable plugins differ from the pins",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := marketplaceCatalog("0.7.17")
			if tt.catalog != nil {
				tt.catalog(catalog)
			}
			h := newPluginsHost(t, tt.marketplaces, catalog, tt.state, tt.settings)
			out, err := h.plugins("install", digest)
			calls := h.calls()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(out, tt.wantErr) {
					t.Fatalf("install = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("install failed: %v\n%s\ncalls:\n%s", err, out, strings.Join(calls, "\n"))
			}
			for _, want := range tt.wantCalls {
				if !slices.Contains(calls, want) {
					t.Errorf("calls lack %q:\n%s", want, strings.Join(calls, "\n"))
				}
			}
			for _, absent := range tt.absentCalls {
				if slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, absent) }) {
					t.Errorf("calls include %q:\n%s", absent, strings.Join(calls, "\n"))
				}
			}
			if got := h.declared("claude-plugins-official"); !reflect.DeepEqual(got, heldOfficial) {
				t.Errorf("settings declare claude-plugins-official as %v, want %v", got, heldOfficial)
			}
			if stamp, err := os.ReadFile(filepath.Join(h.home, ".cc-remote", "ready")); err != nil || string(stamp) != digest+"\n" {
				t.Errorf("ready stamp = %q, %v", stamp, err)
			}
		})
	}
}

func TestPluginsHoldBranchMarketplaceAgainstAutoUpdate(t *testing.T) {
	tests := []struct {
		name        string
		reenable    bool
		wantVersion string
		wantErr     string
	}{
		{name: "held", wantVersion: "0.7.17"},
		{name: "auto-update turned back on", reenable: true, wantVersion: "0.7.18", wantErr: "is not declared in Claude settings at branch main with auto-update off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPluginsHost(t, []Marketplace{toolsRef, officialBranch}, marketplaceCatalog("0.7.17"), fakeState{}, nil)
			if out, err := h.plugins("install", digest); err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if tt.reenable {
				settings := filepath.Join(h.home, ".claude", "settings.json")
				out, err := h.run("jq", "-c", `.extraKnownMarketplaces["claude-plugins-official"].autoUpdate = true`, settings)
				if err != nil {
					t.Fatalf("jq: %v\n%s", err, out)
				}
				if err := os.WriteFile(settings, []byte(out), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.setCatalog(marketplaceCatalog("0.7.18"))
			if out, err := h.run(filepath.Join(h.fakes, "claude"), "auto-update"); err != nil {
				t.Fatalf("auto-update: %v\n%s", err, out)
			}
			if got := h.version("datadog@claude-plugins-official"); got != tt.wantVersion {
				t.Errorf("datadog after the auto-update pass = %q, want %q", got, tt.wantVersion)
			}
			out, err := h.plugins("verify")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verify failed: %v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tt.wantErr) {
				t.Fatalf("verify = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
			}
			out, err = h.plugins("install", digest)
			if err == nil || !strings.Contains(out, "the installed, enabled and loadable plugins differ from the pins") {
				t.Fatalf("reinstall = %v\n%s\nwant the pin check to fail closed", err, out)
			}
			if got := h.declared("claude-plugins-official"); !reflect.DeepEqual(got, heldOfficial) {
				t.Errorf("reinstall left claude-plugins-official declared as %v, want %v", got, heldOfficial)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
