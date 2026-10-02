package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/workspace"
)

func TestPayloadBuildTakesTheSelectionFlags(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"payload", "build"})
	if err != nil || cmd.Name() != "build" {
		t.Fatalf("no payload build command: %v", err)
	}
	for _, flag := range []string{"config", "provider", "profile", "out"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("payload build lacks --%s", flag)
		}
	}
}

func TestPayloadBuildRejectsIncompleteRequests(t *testing.T) {
	out := filepath.Join(t.TempDir(), "payload.sqfs")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"without out", []string{"payload", "build"}, `required flag(s) "out" not set`},
		{"without a config", []string{"payload", "build", "--out", out, "--config", filepath.Join(t.TempDir(), "none.yaml")}, "read config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetOut(io.Discard)
			root.SetArgs(tt.args)
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("stat %s = %v, want it absent", out, err)
			}
		})
	}
}

func TestBuiltPayloadPrintsTheBuildAndItsPath(t *testing.T) {
	built := builtPayload{
		PayloadBuild: workspace.PayloadBuild{SHA256: strings.Repeat("a", 64), Size: 4, Tools: strings.Repeat("b", 64), Machine: "cc-remote-payload-0123abcd"},
		Path:         "/payloads/tools.sqfs",
	}
	var out bytes.Buffer
	if err := emit(&out, built); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"sha256\": \"" + built.SHA256 + "\",\n  \"size\": 4,\n  \"tools\": \"" + built.Tools + "\",\n  \"machine\": \"cc-remote-payload-0123abcd\",\n  \"path\": \"/payloads/tools.sqfs\"\n}\n"
	if got := out.String(); got != want {
		t.Errorf("emit = %q, want %q", got, want)
	}
}

func TestWritePayload(t *testing.T) {
	failed := errors.New("the build broke")
	built := workspace.PayloadBuild{SHA256: strings.Repeat("a", 64), Size: 4, Tools: strings.Repeat("b", 64), Machine: "cc-remote-payload-0123abcd"}
	tests := []struct {
		name     string
		dirMode  fs.FileMode
		existing string
		err      error
		want     workspace.PayloadBuild
		is       error
		refused  bool
		contents string
		calls    int
	}{
		{name: "writes the payload", dirMode: 0o700, want: built, contents: "hsqs", calls: 1},
		{name: "writes into a directory others can only read", dirMode: 0o755, want: built, contents: "hsqs", calls: 1},
		{name: "removes a failed payload", dirMode: 0o700, err: failed, is: failed, calls: 1},
		{name: "keeps an existing file", dirMode: 0o700, existing: "keep", is: fs.ErrExist, contents: "keep"},
		{name: "refuses a group-writable directory", dirMode: 0o770, refused: true},
		{name: "refuses a world-writable directory", dirMode: 0o707, refused: true},
		{name: "refuses a sticky shared directory", dirMode: 0o777 | fs.ModeSticky, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, tt.dirMode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "payload.sqfs")
			if tt.existing != "" {
				if err := os.WriteFile(path, []byte(tt.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			got, err := writePayload(path, func(w io.Writer) (workspace.PayloadBuild, error) {
				calls++
				if _, err := io.WriteString(w, "hsqs"); err != nil {
					return workspace.PayloadBuild{}, err
				}
				return built, tt.err
			})
			refusal := fmt.Sprintf("refusing to write the payload into %s: its mode %v lets its group or other users replace the private payload, so choose a directory only you can write", dir, tt.dirMode.Perm())
			switch {
			case tt.refused:
				if err == nil || err.Error() != refusal {
					t.Errorf("writePayload = %v, want %q", err, refusal)
				}
			case tt.is == nil && err != nil || tt.is != nil && !errors.Is(err, tt.is):
				t.Errorf("writePayload = %v, want %v", err, tt.is)
			}
			if got != tt.want || calls != tt.calls {
				t.Errorf("writePayload = %+v after %d builds, want %+v after %d", got, calls, tt.want, tt.calls)
			}
			contents, err := os.ReadFile(path)
			switch {
			case tt.contents == "":
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("read %s = %q, %v, want it removed", path, contents, err)
				}
			case err != nil || string(contents) != tt.contents:
				t.Errorf("read %s = %q, %v, want %q", path, contents, err, tt.contents)
			}
			if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
				t.Errorf("%s has mode %v, want 0600", path, info.Mode().Perm())
			}
		})
	}
}
