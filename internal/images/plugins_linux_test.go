package images

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fakeClaude = `#!/usr/bin/env python3
import json
import os
import sys

state_path = os.environ["FAKE_STATE"]
catalog = json.load(open(os.environ["FAKE_CATALOG"]))
state = json.load(open(state_path))
args = sys.argv[1:]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("claude " + " ".join(args) + "\n")


def save():
    json.dump(state, open(state_path, "w"))


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

func TestPluginsRegisterMarketplacesByRefAndBranch(t *testing.T) {
	official := Marketplace{Name: "claude-plugins-official", GitHub: "anthropics/claude-plugins-official", Branch: "main"}
	tools := Marketplace{Name: "tools-market", GitHub: "owner/tools-market", Ref: commit}
	catalog := map[string]any{
		"dir:tools-market": map[string]any{"name": "tools-market", "plugins": map[string]string{"hook": "1.0.0"}},
		"github:anthropics/claude-plugins-official#main": map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.17"}},
		"github:anthropics/claude-plugins-official":      map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.17"}},
	}
	healthy := func(id, version string) map[string]any {
		return map[string]any{"id": id, "version": version, "enabled": true, "errors": []any{}}
	}
	tests := []struct {
		name         string
		marketplaces []Marketplace
		catalog      func(map[string]any)
		state        fakeState
		wantErr      string
		wantCalls    []string
		absentCalls  []string
	}{
		{
			name:         "fresh host",
			marketplaces: []Marketplace{tools, official},
			wantCalls: []string{
				"claude plugin marketplace add HOME/.local/share/cc-remote/marketplaces/tools-market",
				"claude plugin marketplace add anthropics/claude-plugins-official#main",
				"claude plugin install datadog@claude-plugins-official",
			},
			absentCalls: []string{"git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "official added without a ref",
			marketplaces: []Marketplace{tools, official},
			state: fakeState{
				Marketplaces: []map[string]any{
					{"name": "tools-market", "source": "directory", "path": "HOME/.local/share/cc-remote/marketplaces/tools-market", "key": "dir:tools-market", "snapshot": map[string]string{"hook": "1.0.0"}},
					{"name": "claude-plugins-official", "source": "github", "repo": "anthropics/claude-plugins-official", "key": "github:anthropics/claude-plugins-official", "snapshot": map[string]string{"datadog": "0.7.17"}},
				},
				Plugins: []map[string]any{healthy("hook@tools-market", "1.0.0"), healthy("datadog@claude-plugins-official", "0.7.17")},
			},
			wantCalls: []string{
				"claude plugin marketplace remove claude-plugins-official",
				"claude plugin marketplace add anthropics/claude-plugins-official#main",
				"claude plugin install datadog@claude-plugins-official",
			},
		},
		{
			name:         "official already pinned",
			marketplaces: []Marketplace{tools, official},
			state: fakeState{
				Marketplaces: []map[string]any{
					{"name": "tools-market", "source": "directory", "path": "HOME/.local/share/cc-remote/marketplaces/tools-market", "key": "dir:tools-market", "snapshot": map[string]string{"hook": "1.0.0"}},
					{"name": "claude-plugins-official", "source": "github", "repo": "anthropics/claude-plugins-official", "ref": "main", "key": "github:anthropics/claude-plugins-official#main", "snapshot": map[string]string{"datadog": "0.7.17"}},
				},
				Plugins: []map[string]any{healthy("hook@tools-market", "1.0.0"), healthy("datadog@claude-plugins-official", "0.7.17")},
			},
			absentCalls: []string{"claude plugin marketplace add", "claude plugin marketplace remove", "claude plugin install", "claude plugin update", "git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "pinned official refreshed by update",
			marketplaces: []Marketplace{tools, official},
			state: fakeState{
				Marketplaces: []map[string]any{
					{"name": "tools-market", "source": "directory", "path": "HOME/.local/share/cc-remote/marketplaces/tools-market", "key": "dir:tools-market", "snapshot": map[string]string{"hook": "1.0.0"}},
					{"name": "claude-plugins-official", "source": "github", "repo": "anthropics/claude-plugins-official", "ref": "main", "key": "github:anthropics/claude-plugins-official#main", "snapshot": map[string]string{"datadog": "0.7.16"}},
				},
				Plugins: []map[string]any{healthy("hook@tools-market", "1.0.0"), healthy("datadog@claude-plugins-official", "0.7.16")},
			},
			wantCalls: []string{
				"claude plugin marketplace update claude-plugins-official",
				"claude plugin update datadog@claude-plugins-official",
			},
			absentCalls: []string{"claude plugin marketplace remove", "git init -q HOME/.local/share/cc-remote/marketplaces/claude-plugins-official"},
		},
		{
			name:         "official from a local checkout",
			marketplaces: []Marketplace{tools, {Name: "claude-plugins-official", GitHub: "anthropics/claude-plugins-official", Ref: commit}},
			catalog: func(c map[string]any) {
				c["dir:claude-plugins-official"] = map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.17"}}
			},
			wantErr: "can only be used with GitHub sources from the 'anthropics' org",
		},
		{
			name:         "upstream bump",
			marketplaces: []Marketplace{tools, official},
			catalog: func(c map[string]any) {
				c["github:anthropics/claude-plugins-official#main"] = map[string]any{"name": "claude-plugins-official", "plugins": map[string]string{"datadog": "0.7.18"}}
			},
			wantErr: "the installed, enabled and loadable plugins differ from the pins",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			fakes := t.TempDir()
			inventory := Inventory{
				Version: SchemaVersion,
				Claude: Claude{
					Marketplaces: tt.marketplaces,
					Plugins: []Plugin{
						{ID: "hook@tools-market", Version: "1.0.0"},
						{ID: "datadog@claude-plugins-official", Version: "0.7.17"},
					},
				},
			}
			scripts, err := Render(inventory, "agents")
			if err != nil {
				t.Fatal(err)
			}
			entries := map[string]any{}
			for key, value := range catalog {
				entries[key] = value
			}
			if tt.catalog != nil {
				tt.catalog(entries)
			}
			state := tt.state
			if state.Marketplaces == nil {
				state.Marketplaces = []map[string]any{}
			}
			if state.Plugins == nil {
				state.Plugins = []map[string]any{}
			}
			for _, m := range state.Marketplaces {
				if path, ok := m["path"].(string); ok {
					m["path"] = strings.Replace(path, "HOME", home, 1)
				}
			}
			files := map[string][]byte{
				"claude":       []byte(fakeClaude),
				"git":          []byte(fakeGit),
				"plugins.sh":   scripts.Plugins,
				"catalog.json": mustJSON(t, entries),
				"state.json":   mustJSON(t, state),
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(fakes, name), data, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(fakes, "calls.log")
			cmd := exec.Command("bash", filepath.Join(fakes, "plugins.sh"), "install", digest)
			cmd.Stdin = strings.NewReader("\n")
			cmd.Env = append(os.Environ(),
				"HOME="+home,
				"PATH="+fakes+":"+os.Getenv("PATH"),
				"FAKE_STATE="+filepath.Join(fakes, "state.json"),
				"FAKE_CATALOG="+filepath.Join(fakes, "catalog.json"),
				"FAKE_LOG="+log,
			)
			out, err := cmd.CombinedOutput()
			raw, _ := os.ReadFile(log)
			calls := strings.Split(strings.ReplaceAll(string(raw), home, "HOME"), "\n")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(string(out), tt.wantErr) {
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
			if stamp, err := os.ReadFile(filepath.Join(home, ".cc-remote", "ready")); err != nil || string(stamp) != digest+"\n" {
				t.Errorf("ready stamp = %q, %v", stamp, err)
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
