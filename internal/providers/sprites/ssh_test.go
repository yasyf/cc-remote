package sprites

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	tests := []struct {
		name      string
		existing  bool
		answers   bool
		wantScans int
		wantErr   string
	}{
		{name: "an existing sshd service is checked but never recreated", existing: true, answers: true, wantScans: 1},
		{name: "an existing sshd service that never answers", existing: true, wantScans: 60, wantErr: "cc-remote: sshd did not accept a connection on port 22 within 30s"},
		{name: "a new sshd service answers", answers: true, wantScans: 1},
		{name: "a new sshd service that never answers", wantScans: 60, wantErr: "cc-remote: sshd did not accept a connection on port 22 within 30s"},
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
				"sleep 0.5", "sleep 0.01",
			).Replace(AuthorizeScript)
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+fakes)
			cmd.Stdin = strings.NewReader(clientKey)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			scans := loggedLines(t, filepath.Join(fakes, "ssh-keyscan.log"))
			if len(scans) != tt.wantScans || slices.ContainsFunc(scans, func(scan string) bool { return scan != wantScan }) {
				t.Errorf("ssh-keyscan ran %d times with %q, want %d times with %q", len(scans), scans, tt.wantScans, wantScan)
			}
			created := loggedLines(t, filepath.Join(fakes, "create.argv"))
			if tt.existing {
				if created != nil {
					t.Errorf("an existing sshd service was created again: %q", created)
				}
			} else if !slices.Equal(created, wantCreate) {
				t.Errorf("create = %q, want %q", created, wantCreate)
			}
			if tt.wantErr != "" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(stderr.String(), tt.wantErr) || len(out) != 0 {
					t.Fatalf("authorize = %v, stdout %q, stderr %q; want exit 1 printing %q and no host key", err, out, stderr.String(), tt.wantErr)
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
