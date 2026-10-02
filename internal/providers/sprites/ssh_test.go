package sprites

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	fakeSpriteEnv = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIR/sprite-env.log"
case "$2" in
  get) test -f "$FAKE_DIR/sshd-service" ;;
  create) printf '%s\n' "$@" > "$FAKE_DIR/create.argv" && : > "$FAKE_DIR/sshd-service" ;;
esac
`
	fakeKeyscan = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIR/ssh-keyscan.log"
[ -z "${FAKE_SCAN_STALL:-}" ] || exec sleep "$FAKE_SCAN_STALL"
sleep "${FAKE_SCAN_DELAY:-0}"
if [ -f "$FAKE_DIR/answers" ]; then
  echo "127.0.0.1 ` + fakeHostKey + `"
else
  echo "connect: Connection refused" >&2
fi
`
)

func TestAuthorizeScriptWaitsForSSHDToAnswer(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", AuthorizeScript).CombinedOutput(); err != nil {
		t.Fatalf("authorize script: %v: %s", err, out)
	}
	wantCreate := []string{"services", "create", "sshd", "--cmd", "sudo", "--args", "sh,-c,mkdir -p /run/sshd && exec /usr/sbin/sshd -D -e", "--duration", "1ms", "--no-stream"}
	wantScan := "-T 1 -t ed25519 127.0.0.1"
	clientKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeClientKey cc-remote alpha\n"
	const deadline = time.Second
	noAnswer := regexp.MustCompile(`cc-remote: sshd did not accept a connection on port 22 in [0-9]+\.[0-9]s \(deadline 1s\)`)
	tests := []struct {
		name      string
		existing  bool
		answers   bool
		blocks    bool
		stall     bool
		probe     time.Duration
		wantScans int
		wantErr   bool
	}{
		{name: "an existing sshd service is checked but never recreated", existing: true, answers: true, wantScans: 1},
		{name: "an existing sshd service that never answers", existing: true, wantErr: true},
		{name: "a new sshd service answers", answers: true, wantScans: 1},
		{name: "a new sshd service that never answers", wantErr: true},
		{name: "a keyscan that blocks for its own timeout never answers", blocks: true, probe: 300 * time.Millisecond, wantErr: true},
		{name: "a keyscan that stalls past the deadline is cut off and never answers", stall: true, probe: 2 * time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakes, home := t.TempDir(), t.TempDir()
			for name, body := range map[string]string{"sprite-env": fakeSpriteEnv, "ssh-keyscan": fakeKeyscan, "sshd": "#!/bin/sh\n"} {
				if err := os.WriteFile(filepath.Join(fakes, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			hostKey := filepath.Join(fakes, "ssh_host_ed25519_key.pub")
			if err := os.WriteFile(hostKey, []byte(fakeHostKey+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for flag, set := range map[string]bool{"sshd-service": tt.existing, "answers": tt.answers} {
				if set {
					if err := os.WriteFile(filepath.Join(fakes, flag), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			script := strings.NewReplacer(
				"test -x /usr/sbin/sshd", "test -x "+filepath.Join(fakes, "sshd"),
				"cat /etc/ssh/ssh_host_ed25519_key.pub", "cat "+hostKey,
				"deadline = 30.0", "deadline = 1.0",
				"time.sleep(0.5)", "time.sleep(0.01)",
			).Replace(AuthorizeScript)
			delay, stall := "0", ""
			if tt.blocks {
				delay = "0.3"
			}
			if tt.stall {
				stall = "10"
			}
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+fakes, "FAKE_SCAN_DELAY="+delay, "FAKE_SCAN_STALL="+stall)
			cmd.Stdin = strings.NewReader(clientKey)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			started := time.Now()
			out, err := cmd.Output()
			elapsed := time.Since(started)
			scans := loggedLines(t, filepath.Join(fakes, "ssh-keyscan.log"))
			if len(scans) == 0 || (tt.wantScans != 0 && len(scans) != tt.wantScans) || slices.ContainsFunc(scans, func(scan string) bool { return scan != wantScan }) {
				t.Errorf("ssh-keyscan ran %d times with %q, want %d times (0 = any) with %q", len(scans), scans, tt.wantScans, wantScan)
			}
			created := loggedLines(t, filepath.Join(fakes, "create.argv"))
			if tt.existing {
				if created != nil {
					t.Errorf("an existing sshd service was created again: %q", created)
				}
			} else if !slices.Equal(created, wantCreate) {
				t.Errorf("create = %q, want %q", created, wantCreate)
			}
			if tt.wantErr {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || !noAnswer.MatchString(stderr.String()) || len(out) != 0 {
					t.Fatalf("authorize = %v, stdout %q, stderr %q; want exit 1 printing %v and no host key", err, out, stderr.String(), noAnswer)
				}
				if elapsed < deadline || elapsed > deadline+tt.probe+2*time.Second {
					t.Errorf("authorize gave up after %v, want within [%v, %v]", elapsed, deadline, deadline+tt.probe+2*time.Second)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorize = %v: %s", err, stderr.String())
			}
			if string(out) != fakeHostKey+"\n" {
				t.Errorf("authorize printed %q, want the host key", out)
			}
			authorized, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
			if err != nil || string(authorized) != clientKey {
				t.Errorf("authorized_keys = %q, %v", authorized, err)
			}
		})
	}
}

func loggedLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
