package images

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestT64LoadsPackageCacheOnce(t *testing.T) {
	records := map[string]string{
		"libalphat64": "Package: libalphat64\nVersion: 1\nProvides: libbetat64\nDescription: fixture\n Package: libbetat64\n\nPackage: libalphat64\nVersion: 2\n\n",
		"libgammat64": "Package: libgammat64\nVersion: 1\n\n",
	}
	for _, tt := range []struct {
		name      string
		args      []string
		records   map[string]string
		cacheExit string
		want      string
	}{
		{name: "mixed aliases and regular names", args: []string{"libalpha", "libbeta", "libgamma"}, records: records, want: "libalphat64\nlibbeta\nlibgammat64\n"},
		{name: "all aliases missing", args: []string{"libbeta", "libdelta"}, records: map[string]string{}, want: "libbeta\nlibdelta\n"},
		{name: "successful empty metadata", args: []string{"libbeta", "libdelta"}, records: map[string]string{}, cacheExit: "0", want: "libbeta\nlibdelta\n"},
		{name: "empty input", records: map[string]string{}},
		{name: "duplicate input order", args: []string{"libgamma", "libalpha", "libgamma", "libbeta"}, records: records, want: "libgammat64\nlibalphat64\nlibgammat64\nlibbeta\n"},
		{name: "cache failure with partial headers", args: []string{"libalpha", "libgamma"}, records: records, cacheExit: "37", want: "libalpha\nlibgamma\n"},
		{name: "cache failure without metadata", args: []string{"libbeta", "libdelta"}, records: map[string]string{}, cacheExit: "37", want: "libbeta\nlibdelta\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scripts, err := Render(Inventory{Version: SchemaVersion}, "agents")
			if err != nil {
				t.Fatal(err)
			}
			provision := string(scripts.ProvisionScript)
			start := strings.Index(provision, "t64() {\n")
			if start < 0 {
				t.Fatal("rendered provision script has no t64 helper")
			}
			end := strings.Index(provision[start:], "\n}\n")
			if end < 0 {
				t.Fatal("rendered t64 helper is incomplete")
			}
			helper := provision[start : start+end+len("\n}\n")]
			root := t.TempDir()
			log := filepath.Join(root, "cache-calls.jsonl")
			fake := `#!/usr/bin/env python3
import json
import os
import sys

args = sys.argv[1:]
with open(os.environ["CACHE_CALLS"], "a") as log:
    log.write(json.dumps(args) + "\n")
if args[0] != "show":
    sys.exit(91)
records = json.loads(os.environ["CACHE_RECORDS"])
found = False
for name in reversed(args[1:]):
    if name in records:
        sys.stdout.write(records[name])
        found = True
sys.stderr.write("fixture cache diagnostic\n")
forced = os.environ["CACHE_EXIT"]
sys.exit(int(forced) if forced else (0 if found else 100))
`
			if err := os.WriteFile(filepath.Join(root, "apt-cache"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(tt.records)
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-c", "set -euo pipefail\n" + helper + "t64 \"$@\"\n", "t64-fixture"}, tt.args...)
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "CACHE_CALLS="+log, "CACHE_RECORDS="+string(encoded), "CACHE_EXIT="+tt.cacheExit)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("t64 failed: %v %s", err, stderr.String())
			}
			if got := stdout.String(); got != tt.want {
				t.Errorf("t64 = %q, want %q", got, tt.want)
			}
			if got := stderr.String(); got != "" {
				t.Errorf("t64 leaked cache diagnostics: %q", got)
			}
			calls, err := os.ReadFile(log)
			if len(tt.args) == 0 {
				if !os.IsNotExist(err) {
					t.Fatalf("empty input invoked apt-cache: %q %v", calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("apt-cache loaded %d times, want one: %q", len(lines), calls)
			}
			var got []string
			if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
				t.Fatal(err)
			}
			want := []string{"show"}
			for _, name := range tt.args {
				want = append(want, name+"t64")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("apt-cache arguments = %v, want %v", got, want)
			}
		})
	}
}
