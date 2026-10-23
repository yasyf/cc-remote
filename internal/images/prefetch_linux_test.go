package images

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrefetchReadsTheMountedImageAndLeavesItAlone(t *testing.T) {
	image := bytes.Repeat([]byte("hsqs"), prefetchBlock/4+1)
	tests := []struct {
		name     string
		image    []byte
		wantExit int
		wantOut  string
	}{
		{name: "an image past one block reads cleanly", image: image},
		{name: "a missing image fails with dd's own error", wantExit: 1, wantOut: "dd: failed to open '%s': No such file or directory\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := t.TempDir()
			path := filepath.Join(store, digest+PayloadArtifact.Suffix)
			if tt.image != nil {
				if err := os.WriteFile(path, tt.image, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.Replace(PayloadArtifact.prefetchScript(), "store="+PayloadArtifact.Store+"\n", "store="+quote(store)+"\n", 1)
			cmd := exec.Command("bash", "-c", script, "cache-payload", digest)
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			out, err := cmd.CombinedOutput()
			exit := 0
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				exit = exited.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if want := strings.ReplaceAll(tt.wantOut, "%s", path); exit != tt.wantExit || string(out) != want {
				t.Fatalf("prefetch exited %d with %q, want %d with %q", exit, out, tt.wantExit, want)
			}
			if tt.image == nil {
				return
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, tt.image) {
				t.Errorf("the prefetch changed the image: %d bytes, %v", len(after), err)
			}
		})
	}
}
