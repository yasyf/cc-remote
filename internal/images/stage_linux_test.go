package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const staleDownload = "hsqs bytes of a cancelled transfer"

var stagedArtifacts = []TransferArtifact{PayloadArtifact, PackagesArtifact}

func TestStageAdmitsOnlyTheExactBytes(t *testing.T) {
	payload, wrong := []byte("hsqs\x00the exact payload bytes\n"), []byte("hsqs\x00the wrong payload bytes\n")
	half := payload[:len(payload)/2]
	digest := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	sha := digest(payload)
	dd, err := exec.LookPath("dd")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		stream  []byte
		fakes   map[string]string
		wantErr string
	}{
		{name: "the exact bytes are admitted", stream: payload},
		{name: "a corrupt stream is refused", stream: wrong, wantErr: " has sha256 " + digest(wrong) + ", want " + sha},
		{name: "a truncated stream is refused", stream: half, wantErr: " has sha256 " + digest(half) + ", want " + sha},
		{name: "an empty stream is refused", stream: nil, wantErr: " has sha256 " + digest(nil) + ", want " + sha},
		{name: "a failed digest cannot admit its matching output", stream: payload, fakes: map[string]string{"openssl": failingHash(sha)}, wantErr: " stream failed (cat exit 0, tee exit 0, dd exit 0, openssl exit 7)"},
		{name: "a failed write cannot admit a matching digest", stream: payload, fakes: map[string]string{"dd": failingWrite(dd)}, wantErr: " stream failed (cat exit 0, tee exit 0, dd exit 5, openssl exit 0)"},
	}
	for _, artifact := range stagedArtifacts {
		for _, tt := range tests {
			t.Run(artifact.Label+"/"+tt.name, func(t *testing.T) {
				store := filepath.Join(t.TempDir(), "store")
				stale := stageStaleDownload(t, store, sha, artifact)
				stage := strings.Replace(artifact.stageScript(), "store="+artifact.Store+"\n", "store="+quote(store)+"\n", 1)
				cmd := exec.Command("bash", "-c", stage, "stage-"+artifact.Label, sha)
				cmd.Stdin = bytes.NewReader(tt.stream)
				if tt.fakes != nil {
					fakes := t.TempDir()
					writeFakes(t, fakes, tt.fakes)
					cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"))
				}
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

func TestStagingWritesDirectlyAndWaitsForTheHash(t *testing.T) {
	const block = 4096
	dd, err := exec.LookPath("dd")
	if err != nil {
		t.Fatal(err)
	}
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		size int
	}{
		{name: "whole blocks", size: 3 * block},
		{name: "whole blocks and a short tail", size: 3*block + 100},
		{name: "less than one block", size: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := make([]byte, tt.size)
			for i := range payload {
				payload[i] = byte(i*7 + i/13)
			}
			sum := sha256.Sum256(payload)
			sha := hex.EncodeToString(sum[:])
			root := t.TempDir()
			store, fakes, writes := filepath.Join(root, "store"), t.TempDir(), filepath.Join(root, "writes")
			writeFakes(t, fakes, map[string]string{
				"dd":      "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(writes) + "\nexec " + quote(dd) + " \"$@\"\n",
				"openssl": "#!/bin/sh\nsleep 0.2\nexec " + quote(openssl) + " \"$@\"\n",
			})
			stage := strings.Replace(PayloadArtifact.stageScript(), "store="+PayloadStore+"\n", "store="+quote(store)+"\n", 1)
			stage = strings.Replace(stage, " bs="+strconv.Itoa(stagingBlock)+" ", " bs="+strconv.Itoa(block)+" ", 1)
			cmd := exec.Command("bash", "-c", stage, "stage-payload", sha)
			cmd.Stdin = bytes.NewReader(payload)
			cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
				t.Fatalf("stage = %v\n%s\nwant a silent success", err, out)
			}
			if got, err := os.ReadFile(filepath.Join(store, sha+".sqfs.admitted")); err != nil || !bytes.Equal(got, payload) {
				t.Errorf("admitted %d bytes, %v; want the %d payload bytes", len(got), err, tt.size)
			}
			recorded, err := os.ReadFile(writes)
			if err != nil {
				t.Fatal(err)
			}
			want := regexp.MustCompile(`^of=` + regexp.QuoteMeta(filepath.Join(store, sha+".sqfs.")) + `[A-Za-z0-9]{8}\.partial bs=` + strconv.Itoa(block) + ` iflag=fullblock oflag=direct status=none\n$`)
			if !want.Match(recorded) {
				t.Errorf("dd ran as %q, want one direct write of the staging file matching %s", recorded, want)
			}
		})
	}
}

func failingHash(sha string) string {
	return "#!/bin/sh\ncat > /dev/null\nsleep 0.2\nprintf '%s  -\\n' " + sha + "\nexit 7\n"
}

func failingWrite(dd string) string {
	return "#!/bin/sh\ncase \"$1\" in of=*) cat > /dev/null; exit 5 ;; esac\nexec " + quote(dd) + " \"$@\"\n"
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
