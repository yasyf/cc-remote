package sprites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yasyf/cc-remote/internal/providers"
)

type keys struct{ dir string }

func (p *Provider) keys() keys { return keys{dir: filepath.Join(p.StateDir, Name, "ssh")} }

func (k keys) identity(id string) string   { return filepath.Join(k.dir, id) }
func (k keys) knownHosts(id string) string { return filepath.Join(k.dir, id+".known_hosts") }
func (k keys) lock(id string) string       { return filepath.Join(k.dir, id+".lock") }

func (k keys) remove(id string) error {
	for _, path := range []string{k.identity(id), k.identity(id) + ".pub", k.knownHosts(id), k.lock(id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func Alias(id string) string { return id + ".sprite.cc-remote" }

func (p *Provider) helperPath() string { return filepath.Join(p.StateDir, "bin", "cc-remote") }

func (p *Provider) Access(ctx context.Context, id string) (providers.Access, error) {
	target, err := p.target(ctx, id)
	if err != nil {
		return providers.Access{}, err
	}
	return providers.Access{Kind: providers.AccessOpenSSH, SSH: target}, nil
}

func (p *Provider) target(ctx context.Context, id string) (providers.Target, error) {
	if _, err := p.Get(ctx, id); err != nil {
		return providers.Target{}, err
	}
	keys := p.keys()
	if err := p.ensureKey(ctx, keys, id); err != nil {
		return providers.Target{}, err
	}
	public, err := os.ReadFile(keys.identity(id) + ".pub")
	if err != nil {
		return providers.Target{}, err
	}
	result, err := p.Exec(ctx, id, []string{"sh", "-c", AuthorizeScript}, bytes.NewReader(public))
	if err != nil {
		return providers.Target{}, err
	}
	if result.ExitCode != 0 {
		return providers.Target{}, fmt.Errorf("authorizing ssh on sprite %s: exit %d: %s", id, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	hostKey := strings.Fields(string(result.Stdout))
	if len(hostKey) < 2 {
		return providers.Target{}, fmt.Errorf("sprite %s printed no ssh host key", id)
	}
	if err := os.WriteFile(keys.knownHosts(id), []byte(Alias(id)+" "+hostKey[0]+" "+hostKey[1]+"\n"), 0o600); err != nil {
		return providers.Target{}, err
	}
	if err := p.installHelper(); err != nil {
		return providers.Target{}, err
	}
	cli, err := exec.LookPath(p.CLI)
	if err != nil {
		return providers.Target{}, fmt.Errorf("sprite CLI %q not found; install it from https://sprites.dev: %w", p.CLI, err)
	}
	return providers.Target{
		Host:         id,
		Port:         22,
		User:         user,
		IdentityFile: keys.identity(id),
		ProxyCommand: providers.ShellQuote(p.helperPath(), "proxy", "--", cli, "proxy", "-o", p.Org, "-s", id, "-W", ":22"),
		HostKeyPolicy: providers.HostKeyPolicy{
			Mode:           providers.HostKeyPinned,
			Alias:          Alias(id),
			KnownHostsFile: keys.knownHosts(id),
		},
	}, nil
}

func (p *Provider) ensureKey(ctx context.Context, keys keys, id string) (err error) {
	if err := os.MkdirAll(keys.dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(keys.lock(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if _, err := os.Stat(keys.identity(id)); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err = providers.Output(ctx, p.Runner, providers.Command{
		Name: "ssh-keygen",
		Args: []string{"-q", "-t", "ed25519", "-N", "", "-C", "cc-remote " + id, "-f", keys.identity(id)},
	})
	return err
}

func (p *Provider) installHelper() error {
	helper, err := os.ReadFile(p.Helper)
	if err != nil {
		return err
	}
	path := p.helperPath()
	if installed, err := os.ReadFile(path); err == nil && bytes.Equal(installed, helper) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return providers.WriteFileAtomic(path, helper, 0o700)
}

func Proxy(ctx context.Context, argv []string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, closed := context.WithCancel(ctx)
	defer closed()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(stdin, os.Stdin)
		closed()
	}()
	if err := cmd.Wait(); ctx.Err() == nil {
		return err
	}
	return nil
}

const AuthorizeScript = `set -eu
test -x /usr/sbin/sshd || { echo "cc-remote: this sprite has no /usr/sbin/sshd; provision openssh-server before asking for ssh" >&2; exit 1; }
key="$(cat)"
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
touch "$HOME/.ssh/authorized_keys"
grep -qxF "$key" "$HOME/.ssh/authorized_keys" || printf '%s\n' "$key" >> "$HOME/.ssh/authorized_keys"
chmod 600 "$HOME/.ssh/authorized_keys"
sprite-env services get sshd >/dev/null 2>&1 || sprite-env services create sshd --cmd sudo --args "sh,-c,mkdir -p /run/sshd && exec /usr/sbin/sshd -D -e" --duration 1ms --no-stream >&2
python3 - <<'PY'
import subprocess
import sys
import time

deadline = 30.0
start = time.monotonic()
while True:
    try:
        answer = subprocess.run(["ssh-keyscan", "-T", "1", "-t", "ed25519", "127.0.0.1"], capture_output=True, timeout=max(deadline - (time.monotonic() - start), 2)).stdout
    except subprocess.TimeoutExpired:
        answer = b""
    if answer.strip():
        break
    elapsed = time.monotonic() - start
    if elapsed >= deadline:
        print(f"cc-remote: sshd did not accept a connection on port 22 in {elapsed:.1f}s (deadline {deadline:g}s)", file=sys.stderr)
        sys.exit(1)
    time.sleep(0.5)
PY
cat /etc/ssh/ssh_host_ed25519_key.pub`
