package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const staleDownload = "hsqs bytes of a cancelled transfer"

var stagedArtifacts = []TransferArtifact{PayloadArtifact, PackagesArtifact}

func TestStageAdmitsOnlyTheExactBytes(t *testing.T) {
	payload, wrong := []byte("hsqs the exact payload bytes"), []byte("hsqs the wrong payload bytes")
	half := payload[:len(payload)/2]
	digest := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	sha := digest(payload)
	tests := []struct {
		name    string
		stream  []byte
		wantErr string
	}{
		{name: "the exact bytes are admitted", stream: payload},
		{name: "a corrupt stream is refused", stream: wrong, wantErr: " has sha256 " + digest(wrong) + ", want " + sha},
		{name: "a truncated stream is refused", stream: half, wantErr: " has sha256 " + digest(half) + ", want " + sha},
		{name: "an empty stream is refused", stream: nil, wantErr: " has sha256 " + digest(nil) + ", want " + sha},
	}
	for _, artifact := range stagedArtifacts {
		for _, tt := range tests {
			t.Run(artifact.Label+"/"+tt.name, func(t *testing.T) {
				store := filepath.Join(t.TempDir(), "store")
				stale := stageStaleDownload(t, store, sha, artifact)
				stage := strings.Replace(artifact.stageScript(), "store="+artifact.Store+"\n", "store="+quote(store)+"\n", 1)
				cmd := exec.Command("bash", "-c", stage, "stage-"+artifact.Label, sha)
				cmd.Stdin = bytes.NewReader(tt.stream)
				out, err := cmd.CombinedOutput()
				assertOnlyTheStaleDownloadRemains(t, store, stale)
				admitted := filepath.Join(store, sha+artifact.Suffix+".admitted")
				if tt.wantErr != "" {
					if want := "cc-remote: the " + artifact.Name + tt.wantErr; exitCode(err) != 1 || !strings.Contains(string(out), want) {
						t.Fatalf("stage = %v\n%s\nwant exit 1 with %q", err, out, want)
					}
					if _, err := os.Stat(admitted); !os.IsNotExist(err) {
						t.Errorf("a failed stream was admitted: %v", err)
					}
					return
				}
				if err != nil || len(out) != 0 {
					t.Fatalf("stage = %v\n%s\nwant a silent success", err, out)
				}
				if got, err := os.ReadFile(admitted); err != nil || !bytes.Equal(got, payload) {
					t.Errorf("admitted %q, %v; want the %s", got, err, artifact.Name)
				}
			})
		}
	}
}

func stageStaleDownload(t *testing.T, store, sha string, artifact TransferArtifact) string {
	t.Helper()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(store, sha+artifact.Suffix+".partial")
	if err := os.WriteFile(stale, []byte(staleDownload), 0o644); err != nil {
		t.Fatal(err)
	}
	return stale
}

func assertOnlyTheStaleDownloadRemains(t *testing.T, store, stale string) {
	t.Helper()
	partials, err := filepath.Glob(filepath.Join(store, "*.partial"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(partials, []string{stale}) {
		t.Errorf("the store holds %q, want only the stale transfer %q", partials, stale)
	}
	if got, err := os.ReadFile(stale); err != nil || string(got) != staleDownload {
		t.Errorf("the stale transfer became %q, %v; want it untouched", got, err)
	}
}
