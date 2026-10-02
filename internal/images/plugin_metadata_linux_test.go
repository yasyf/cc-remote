package images

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationReadsMetadataOnceAndRunsEveryProbe(t *testing.T) {
	h := metadataHost(t, nil)
	if out, err := h.plugins("install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := metadataReadCount(h.calls()); got != 18 {
		t.Fatalf("install metadata reads = %d, want 18", got)
	}
	state := h.state()
	for _, plugin := range state.Plugins[:7] {
		root := plugin["installPath"].(string)
		name, _, _ := strings.Cut(plugin["id"].(string), "@")
		writePluginTestFile(t, filepath.Join(root, "bin", name), []byte("#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$FAKE_LOG.probes\"\n"), 0o755)
	}
	writePluginTestFile(t, filepath.Join(h.fakes, "calls.log"), nil, 0o600)
	if out, err := h.plugins("verify"); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	for _, call := range []string{"claude plugin list --json", "claude plugin marketplace list --json"} {
		if got := countCall(h.calls(), call); got != 1 {
			t.Errorf("%s calls = %d, want 1", call, got)
		}
	}
	probes, err := os.ReadFile(filepath.Join(h.fakes, "calls.log.probes"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(probes)), "\n")); got != 7 {
		t.Fatalf("runtime probes = %d, want 7: %s", got, probes)
	}
	writePluginTestFile(t, filepath.Join(state.Plugins[0]["installPath"].(string), "bin/tool-00"), []byte("#!/bin/sh\nexit 73\n"), 0o755)
	if out, err := h.plugins("verify"); exitCode(err) != 73 {
		t.Fatalf("probe exit = %v\n%s, want 73", err, out)
	}
}

func TestVerificationUsesCurrentMetadataOnEveryInvocation(t *testing.T) {
	for _, change := range []string{"version", "disabled", "errors", "path", "duplicate-id", "marketplace"} {
		t.Run(change, func(t *testing.T) {
			h := metadataHost(t, nil)
			if out, err := h.plugins("install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			state := h.state()
			switch change {
			case "version":
				state.Plugins[0]["version"] = "9.0.0"
			case "disabled":
				settings := h.settings()
				settings["enabledPlugins"].(map[string]any)[state.Plugins[0]["id"].(string)] = false
				writePluginTestFile(t, filepath.Join(h.home, ".claude", "settings.json"), mustJSON(t, settings), 0o600)
			case "errors":
				state.Plugins[0]["errors"] = []string{"unloadable"}
			case "path":
				state.Plugins[0]["installPath"] = "/missing/plugin"
			case "duplicate-id":
				state.Plugins = append(state.Plugins, state.Plugins[0])
			case "marketplace":
				state.Marketplaces[0]["ref"] = "other"
			}
			writePluginTestFile(t, filepath.Join(h.fakes, "state.json"), mustJSON(t, state), 0o600)
			if out, err := h.plugins("verify"); err == nil {
				t.Fatalf("verify accepted current %s metadata: %s", change, out)
			}
		})
	}
}

func TestFinalVerificationReadsAfterPrepare(t *testing.T) {
	for _, mutation := range []string{`data["plugins"][0]["version"] = "9.0.0"`, `data["marketplaces"][0]["ref"] = "other"`} {
		t.Run(mutation, func(t *testing.T) {
			prepare := "python3 -c " + quote(`import json, os; path = os.environ["FAKE_STATE"]; data = json.load(open(path)); `+mutation+`; json.dump(data, open(path, "w"))`)
			h := metadataHost(t, []string{prepare})
			if out, err := h.plugins("install"); err == nil {
				t.Fatalf("install accepted metadata changed after plugin installation: %s", out)
			}
			h.unready()
		})
	}
}

func metadataHost(t *testing.T, prepare []string) pluginsHost {
	t.Helper()
	inventory := Inventory{Version: SchemaVersion, Prepare: prepare}
	catalog := map[string]any{}
	for index := range 14 {
		market := fmt.Sprintf("market-%02d", index)
		repo := "owner/" + market
		inventory.Claude.Marketplaces = append(inventory.Claude.Marketplaces, Marketplace{Name: market, GitHub: repo, Branch: "main"})
		versions := map[string]string{}
		for offset := range 2 {
			number := index*2 + offset
			name := fmt.Sprintf("tool-%02d", number)
			plugin := Plugin{ID: name + "@" + market, Version: "1.0.0"}
			if number < 7 {
				plugin.Bins = []string{"bin/" + name}
			}
			inventory.Claude.Plugins = append(inventory.Claude.Plugins, plugin)
			versions[name] = plugin.Version
		}
		catalog["github:"+repo+"#main"] = map[string]any{"name": market, "plugins": versions}
	}
	return newPluginsHost(t, inventory, catalog, fakeState{}, nil)
}

func metadataReadCount(calls []string) int {
	return countCall(calls, "claude plugin list --json") + countCall(calls, "claude plugin marketplace list --json")
}

func countCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}
