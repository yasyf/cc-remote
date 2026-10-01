package images

import (
	"encoding/json"
	"errors"
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
import shutil
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
    tail_path = os.path.join(os.path.dirname(state_path), "list-tail")
    tail = open(tail_path).read() if os.path.exists(tail_path) else ""
    if tail == "garbage":
        print("garbage")
    elif tail == "extra":
        print("[]")
    elif tail == "exit":
        sys.exit(42)
elif args[:3] == ["plugin", "marketplace", "add"]:
    source = args[3]
    if source.startswith("/"):
        if not os.path.isfile(os.path.join(source, ".fake-head")):
            fail("no checkout at " + source)
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
    tail_path = os.path.join(os.path.dirname(state_path), "plugin-list-tail")
    if os.path.exists(tail_path):
        mode, skip, faults = open(tail_path).read().split()
        if int(skip) > 0:
            open(tail_path, "w").write(" ".join([mode, str(int(skip) - 1), faults]))
        elif int(faults) > 0:
            open(tail_path, "w").write(" ".join([mode, skip, str(int(faults) - 1)]))
            if mode == "garbage":
                print("garbage")
            elif mode == "extra":
                print("[]")
            elif mode == "exit":
                sys.exit(42)
elif args[:2] in (["plugin", "install"], ["plugin", "update"]):
    name, _, market = args[2].partition("@")
    source = marketplace(market)
    if not source:
        fail("no marketplace " + market)
    version = source["snapshot"][name]
    root = os.path.join(os.path.dirname(state_path), "plugins", name)
    manifest_path = os.path.join(source.get("path", ""), ".claude-plugin", "marketplace.json")
    if os.path.isfile(manifest_path):
        manifest = json.load(open(manifest_path))
        relative = next(p["source"] for p in manifest["plugins"] if p["name"] == name)
        shutil.copytree(os.path.join(source["path"], relative), root, dirs_exist_ok=True)
    else:
        os.makedirs(os.path.join(root, "bin"), exist_ok=True)
        with open(os.path.join(root, "bin", name), "w") as tool:
            tool.write("#!/bin/sh\n")
        os.chmod(os.path.join(root, "bin", name), 0o755)
    state["plugins"] = [p for p in state["plugins"] if p["id"] != args[2]]
    state["plugins"].append({"id": args[2], "version": version, "enabled": True, "errors": [], "installPath": root})
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
        echo "$6" > "$dir/.fake-fetch"
        ;;
      checkout)
        [ "$3" = FETCH_HEAD ] || exit 2
        cp "$dir/.fake-fetch" "$dir/.fake-head"
        source="$FAKE_SOURCES/$(basename "$dir")"
        if [ -d "$source" ]; then
          cp -R "$source/." "$dir/"
        fi
        ;;
      rev-parse) cat "$dir/.fake-head" 2> /dev/null || exit 128 ;;
      ls-tree | cat-file | hash-object) exec "$REAL_GIT" -C "$dir" "$@" ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`

const fakeSynckitReader = `#!/bin/sh
state=missing
if [ -f "$XDG_CONFIG_HOME/synckit/state.json" ]; then
  state=present
fi
echo "$(basename "$0") $* state=$state" >> "$FAKE_LOG"
[ "$2" != get ]
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

func marketplaceInventory(marketplaces []Marketplace) Inventory {
	return Inventory{
		Version: SchemaVersion,
		Claude: Claude{
			Marketplaces: marketplaces,
			Plugins: []Plugin{
				{ID: "hook@tools-market", Version: "1.0.0"},
				{ID: "datadog@claude-plugins-official", Version: "0.7.17"},
			},
		},
	}
}

func newPluginsHost(t *testing.T, inventory Inventory, catalog map[string]any, state fakeState, settings map[string]any) pluginsHost {
	h := pluginsHost{t: t, home: t.TempDir(), fakes: t.TempDir()}
	scripts, err := Render(inventory, "agents")
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
		"sprite-env": []byte(fakeSynckitReader),
		"cookiesync": []byte(fakeSynckitReader),
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
	git, err := exec.LookPath("git")
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader("\n")
	cmd.Env = append(os.Environ(),
		"HOME="+h.home,
		"XDG_CONFIG_HOME="+filepath.Join(h.home, ".config"),
		"PATH="+h.fakes+":"+os.Getenv("PATH"),
		"FAKE_STATE="+filepath.Join(h.fakes, "state.json"),
		"FAKE_CATALOG="+filepath.Join(h.fakes, "catalog.json"),
		"FAKE_LOG="+filepath.Join(h.fakes, "calls.log"),
		"FAKE_SOURCES="+filepath.Join(h.fakes, "sources"),
		"REAL_GIT="+git,
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

func (h pluginsHost) settings() map[string]any {
	path := filepath.Join(h.home, ".claude", "settings.json")
	info, err := os.Stat(path)
	if err != nil {
		h.t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		h.t.Errorf("settings.json mode = %#o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		h.t.Fatal(err)
	}
	return settings
}

func (h pluginsHost) declared(name string) any {
	declared, _ := h.settings()["extraKnownMarketplaces"].(map[string]any)
	return declared[name]
}

func (h pluginsHost) unready() {
	if _, err := os.Stat(filepath.Join(h.home, ".cc-remote", "ready")); !os.IsNotExist(err) {
		h.t.Errorf("ready stamp after a failed install: %v", err)
	}
}

func (h pluginsHost) state() fakeState {
	raw, err := os.ReadFile(filepath.Join(h.fakes, "state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var state fakeState
	if err := json.Unmarshal(raw, &state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h pluginsHost) editState(edit func(fakeState)) {
	state := h.state()
	edit(state)
	if err := os.WriteFile(filepath.Join(h.fakes, "state.json"), mustJSON(h.t, state), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h pluginsHost) checkedOut(name string) string {
	head, _ := os.ReadFile(filepath.Join(h.home, ".local", "share", "cc-remote", "marketplaces", name, ".fake-head"))
	return strings.TrimSpace(string(head))
}

func (h pluginsHost) version(id string) string {
	for _, p := range h.state().Plugins {
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
	keptEnv := map[string]any{"KEEP": "1"}
	tests := []struct {
		name         string
		marketplaces []Marketplace
		catalog      func(map[string]any)
		state        fakeState
		settings     map[string]any
		loose        bool
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
			settings:     map[string]any{"env": keptEnv, "extraKnownMarketplaces": map[string]any{"claude-plugins-official": declaredOfficial}},
			absentCalls:  []string{"claude plugin marketplace add", "claude plugin marketplace remove", "claude plugin install", "claude plugin update", "git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "pinned official with auto-update turned on",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered("main", "0.7.17"),
			settings:     map[string]any{"env": keptEnv, "extraKnownMarketplaces": map[string]any{"claude-plugins-official": map[string]any{"source": declaredOfficial["source"], "autoUpdate": true}}},
			absentCalls:  []string{"claude plugin marketplace add", "claude plugin marketplace remove"},
		},
		{
			name:         "settings and a stale temp file at 0644",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state:        registered("main", "0.7.17"),
			settings:     map[string]any{"env": keptEnv, "extraKnownMarketplaces": map[string]any{"claude-plugins-official": declaredOfficial}},
			loose:        true,
			absentCalls:  []string{"claude plugin marketplace add", "claude plugin marketplace remove"},
		},
		{
			name:         "tools market switched from branch to ref",
			marketplaces: []Marketplace{toolsRef, officialBranch},
			state: func() fakeState {
				state := registered("main", "0.7.17")
				state.Marketplaces[0] = map[string]any{"name": "tools-market", "source": "github", "repo": "owner/tools-market", "ref": "main", "key": "github:owner/tools-market#main", "snapshot": map[string]string{"hook": "1.0.0"}}
				return state
			}(),
			wantCalls: []string{
				"claude plugin marketplace remove tools-market",
				"claude plugin marketplace add HOME/.local/share/cc-remote/marketplaces/tools-market",
				"claude plugin install hook@tools-market",
			},
			absentCalls: []string{"claude plugin marketplace remove claude-plugins-official"},
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
			h := newPluginsHost(t, marketplaceInventory(tt.marketplaces), catalog, tt.state, tt.settings)
			if tt.loose {
				settings := filepath.Join(h.home, ".claude", "settings.json")
				if err := os.Chmod(settings, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(settings+".tmp", []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(settings+".tmp", 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.plugins("install", digest)
			calls := h.calls()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(out, tt.wantErr) {
					t.Fatalf("install = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
				}
				h.unready()
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
			if got := h.checkedOut("tools-market"); got != commit {
				t.Errorf("tools-market checkout = %q, want %s", got, commit)
			}
			if tt.settings != nil {
				if got := h.settings()["env"]; !reflect.DeepEqual(got, tt.settings["env"]) {
					t.Errorf("settings env = %v, want it kept as %v", got, tt.settings["env"])
				}
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
			h := newPluginsHost(t, marketplaceInventory([]Marketplace{toolsRef, officialBranch}), marketplaceCatalog("0.7.17"), fakeState{}, nil)
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
			h.unready()
			if got := h.declared("claude-plugins-official"); !reflect.DeepEqual(got, heldOfficial) {
				t.Errorf("reinstall left claude-plugins-official declared as %v, want %v", got, heldOfficial)
			}
		})
	}
}

func TestPluginsVerifyRegistrations(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(fakeState)
		wantErr string
	}{
		{
			name: "branch registration moved",
			edit: func(state fakeState) {
				state.Marketplaces[1]["ref"] = "release"
			},
			wantErr: "marketplace claude-plugins-official is not registered from github anthropics/claude-plugins-official main",
		},
		{
			name: "ref registration moved",
			edit: func(state fakeState) {
				state.Marketplaces[0]["path"] = "/elsewhere/tools-market"
			},
			wantErr: "marketplace tools-market is not registered from directory ",
		},
		{
			name: "ref registration replaced by github",
			edit: func(state fakeState) {
				state.Marketplaces[0] = map[string]any{"name": "tools-market", "source": "github", "repo": "owner/tools-market", "ref": "main"}
			},
			wantErr: "marketplace tools-market is not registered from directory ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPluginsHost(t, marketplaceInventory([]Marketplace{toolsRef, officialBranch}), marketplaceCatalog("0.7.17"), fakeState{}, nil)
			if out, err := h.plugins("install", digest); err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if out, err := h.plugins("verify"); err != nil {
				t.Fatalf("verify after install failed: %v\n%s", err, out)
			}
			h.editState(tt.edit)
			out, err := h.plugins("verify")
			if err == nil || !strings.Contains(out, tt.wantErr) {
				t.Fatalf("verify = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
			}
		})
	}
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

func TestPluginsFailClosedOnMarketplaceReads(t *testing.T) {
	tests := []struct {
		tail     string
		wantCode int
		wantOut  string
	}{
		{tail: "exit", wantCode: 42},
		{tail: "garbage", wantCode: 1, wantOut: "did not print exactly one JSON document"},
		{tail: "extra", wantCode: 1, wantOut: "did not print exactly one JSON document"},
	}
	for _, tt := range tests {
		t.Run(tt.tail, func(t *testing.T) {
			h := newPluginsHost(t, marketplaceInventory([]Marketplace{toolsRef, officialBranch}), marketplaceCatalog("0.7.17"), fakeState{}, nil)
			if out, err := h.plugins("install", digest); err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if err := os.WriteFile(filepath.Join(h.fakes, "list-tail"), []byte(tt.tail), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"verify", "install"} {
				out, err := h.plugins(phase, digest)
				if exitCode(err) != tt.wantCode || !strings.Contains(out, tt.wantOut) {
					t.Errorf("%s = %v\n%s\nwant exit %d with %q", phase, err, out, tt.wantCode, tt.wantOut)
				}
			}
			h.unready()
		})
	}
}

func TestPluginsFailClosedOnPluginReads(t *testing.T) {
	tests := []struct {
		name     string
		state    fakeState
		phase    string
		tail     string
		wantCode int
		wantOut  string
	}{
		{name: "plugin list exits non-zero", phase: "verify", tail: "exit 0 9", wantCode: 42},
		{name: "plugin list prints trailing garbage", phase: "verify", tail: "garbage 0 9", wantCode: 1, wantOut: "did not print exactly one JSON document"},
		{name: "plugin list prints a second document", phase: "verify", tail: "extra 0 9", wantCode: 1, wantOut: "did not print exactly one JSON document"},
		{name: "plugin root read exits non-zero", phase: "verify", tail: "exit 1 1", wantCode: 42},
		{name: "install plugin read exits non-zero once", state: registered("main", "0.7.16"), phase: "install", tail: "exit 1 1", wantCode: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := marketplaceInventory([]Marketplace{toolsRef, officialBranch})
			inventory.Claude.Plugins[1].Bins = []string{"bin/datadog"}
			h := newPluginsHost(t, inventory, marketplaceCatalog("0.7.17"), tt.state, nil)
			if tt.phase == "verify" {
				if out, err := h.plugins("install", digest); err != nil {
					t.Fatalf("install failed: %v\n%s", err, out)
				}
				if err := os.Remove(filepath.Join(h.home, ".cc-remote", "ready")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(h.fakes, "plugin-list-tail"), []byte(tt.tail), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := h.plugins(tt.phase, digest)
			if exitCode(err) != tt.wantCode || !strings.Contains(out, tt.wantOut) {
				t.Errorf("%s = %v\n%s\nwant exit %d with %q", tt.phase, err, out, tt.wantCode, tt.wantOut)
			}
			h.unready()
		})
	}
}

func TestPluginsVerifyCaptainHookBuild(t *testing.T) {
	uv := Artifact{Name: "uv", Version: "0.1.0", URL: "https://example.com/uv", SHA256: digest, Format: Binary}
	inventory := Inventory{
		Version:     SchemaVersion,
		Tools:       []Artifact{uv},
		CaptainHook: &CaptainHook{Version: "1.0.0", URL: "https://example.com/hook.tar.gz", SHA256: digest},
	}
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "pinned build", version: `{"build":"1.0.0"}`},
		{name: "trailing garbage", version: `{"build":"1.0.0"}` + "\ngarbage", wantErr: true},
		{name: "second document", version: `{"build":"1.0.0"}` + "\n" + `{"build":"1.0.0"}`, wantErr: true},
		{name: "other build", version: `{"build":"0.9.0"}`, wantErr: true},
		{name: "missing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPluginsHost(t, inventory, nil, fakeState{}, nil)
			tool := filepath.Join(h.home, ".local", "share", "cc-remote", "tools", "uv-0.1.0")
			bin := filepath.Join(h.home, ".local", "bin")
			for _, dir := range []string{tool, bin} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(tool, ".cc-remote-digest"), []byte(digest+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tool, "uv"), []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(tool, "uv"), filepath.Join(bin, "uv")); err != nil {
				t.Fatal(err)
			}
			if tt.version != "" {
				host := filepath.Join(h.home, ".local", "share", "captain-hook", "host")
				if err := os.MkdirAll(host, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(host, "version.json"), []byte(tt.version), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.plugins("verify")
			if (err != nil) != tt.wantErr {
				t.Errorf("verify = %v, want failure %v\n%s", err, tt.wantErr, out)
			}
		})
	}
}

func TestPluginsConfigureWritesSynckitStateBeforeServices(t *testing.T) {
	inventory := Inventory{
		Version:    SchemaVersion,
		Tools:      []Artifact{{Name: "cookiesync", Version: "0.30.0", URL: "https://example.com/cookiesync.tar.gz", SHA256: digest, Format: TarGz, Bins: map[string]string{"cookiesync": "cookiesync"}}},
		Cookiesync: &Cookiesync{SchemaFingerprint: digest},
		Services:   []Service{{Name: "cookiesync", Command: []string{"cookiesync", "supervise"}}},
		Configure:  Configure{Run: []string{"cookiesync check"}},
	}
	pinned := `{"schema":{"identity":"synckit-state-v1","version":1,"fingerprint":"` + digest + `"}}`
	tests := []struct {
		name     string
		existing string
		wantErr  string
	}{
		{name: "fresh host"},
		{name: "existing state at the pinned fingerprint", existing: `{"schema":{"identity":"synckit-state-v1","version":1,"fingerprint":"` + digest + `"},"synckit":{"kept":true}}`},
		{name: "stale state", existing: `{"schema":{"identity":"synckit-state-v1","version":1,"fingerprint":"` + strings.Repeat("3", 64) + `"}}`, wantErr: "is not synckit-state-v1 at schema fingerprint " + digest},
		{name: "wrong identity", existing: `{"schema":{"identity":"broken-state","version":1,"fingerprint":"` + digest + `"}}`, wantErr: "is not synckit-state-v1 at schema fingerprint " + digest},
		{name: "missing identity", existing: `{"schema":{"version":1,"fingerprint":"` + digest + `"}}`, wantErr: "is not synckit-state-v1 at schema fingerprint " + digest},
		{name: "two documents", existing: `{"schema":{"identity":"broken-state"}}` + "\n" + pinned, wantErr: "is not synckit-state-v1 at schema fingerprint " + digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPluginsHost(t, inventory, nil, fakeState{}, nil)
			state := filepath.Join(h.home, ".config", "synckit", "state.json")
			if tt.existing != "" {
				if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(state, []byte(tt.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.plugins("configure")
			calls := h.calls()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(out, tt.wantErr) {
					t.Fatalf("configure = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
				}
				if slices.ContainsFunc(calls, func(call string) bool {
					return strings.HasPrefix(call, "sprite-env services create") || strings.HasPrefix(call, "cookiesync check")
				}) {
					t.Errorf("configure ran a command or started a service on an invalid state:\n%s", strings.Join(calls, "\n"))
				}
				return
			}
			if err != nil {
				t.Fatalf("configure failed: %v\n%s", err, out)
			}
			start := slices.IndexFunc(calls, func(call string) bool {
				return strings.HasPrefix(call, "sprite-env services create cc-remote-cookiesync ")
			})
			check := slices.Index(calls, "cookiesync check state=present")
			install := slices.Index(calls, "cookiesync install state=present")
			if check < 0 || start < check || !strings.HasSuffix(calls[start], " state=present") || install < start {
				t.Errorf("want configure.run and the service to see state.json, then cookiesync install:\n%s", strings.Join(calls, "\n"))
			}
			raw, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			var written struct {
				Schema struct {
					Identity    string `json:"identity"`
					Fingerprint string `json:"fingerprint"`
				} `json:"schema"`
			}
			if err := json.Unmarshal(raw, &written); err != nil || written.Schema.Identity != "synckit-state-v1" || written.Schema.Fingerprint != digest {
				t.Errorf("state.json = %s, %v", raw, err)
			}
			if tt.existing != "" && string(raw) != tt.existing {
				t.Errorf("configure rewrote an existing state.json:\n%s", raw)
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
