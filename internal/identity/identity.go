package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	forgetAgent  = `rm -rf "$HOME"/.claude.json "$HOME"/.claude.json.* "$HOME/.claude/backups"`
	ShareDir     = "$HOME/.local/share/" + remote.Prefix
	tokenPattern = `gh[opsu]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`
)

var credentialFiles = []string{
	`"$HOME/.git-credentials"`,
	`"$HOME/.config/gh/hosts.yml"`,
	`"$HOME/.claude/.credentials.json"`,
	`"$HOME/.codex/auth.json"`,
	`"$HOME/.ssh/authorized_keys"`,
	`/var/lib/tailscale/tailscaled.state`,
	`"` + remote.StateDir + `/tailscaled.state"`,
}

var tokenScan = []string{
	`"$HOME/.config"`,
	`"$HOME/.claude"`,
	`"$HOME/.claude.json"`,
	`"$HOME/.codex"`,
	`"` + remote.StateDir + `"`,
	`"` + ShareDir + `"`,
	`"$root/.git/config"`,
	`"${TMPDIR:-/tmp}"`,
}

func FreshScript(hostKeys bool) string {
	if hostKeys {
		return remote.Script("sudo -n rm -f /etc/ssh/ssh_host_*", "sudo -n ssh-keygen -A", forgetAgent)
	}
	return remote.Script(forgetAgent)
}

func CleanScript(projectRoot, repository, credentialHelper string, forbidden []string) string {
	paths := make([]string, 0, len(credentialFiles)+len(forbidden))
	paths = append(paths, credentialFiles...)
	for _, path := range forbidden {
		paths = append(paths, `"`+path+`"`)
	}
	return `set -eu
root=` + remote.Quote(projectRoot) + `
dirty() { echo "` + remote.Prefix + `: this spare is not clean: $*" >&2; exit 1; }
for path in ` + strings.Join(paths, " ") + `; do
  if [ -e "$path" ]; then dirty "$path exists"; fi
done
for key in "$HOME"/.ssh/id_*; do
  if [ -e "$key" ]; then dirty "$key is a private key"; fi
done
export LC_ALL=C
records="$(mktemp)" lines="$(mktemp)"
trap 'rm -f "$records" "$lines"' EXIT
for config in "$HOME/.gitconfig" "${XDG_CONFIG_HOME:-$HOME/.config}/git/config"; do
  if [ ! -e "$config" ]; then continue; fi
  if [ ! -r "$config" ]; then dirty "$config is unreadable"; fi
  git -C "$root" config --file "$config" --includes --null --get-regexp '^credential\.' >> "$records" || [ $? -eq 1 ]
done
tr -d '\001' < "$records" > "$lines"
if ! cmp -s "$lines" "$records"; then dirty "global git config holds a credential setting with a control character"; fi
tr '\000\n' '\n\001' < "$records" > "$lines"
separator="$(printf '\001')"
while IFS= read -r record; do
  case "$record" in
    *"$separator"*"$separator"*) dirty "global git config holds a credential setting of more than one line" ;;
    *"$separator"*) ;;
    *) dirty "global git config holds a credential setting with no value" ;;
  esac
  key="${record%%"$separator"*}"
  value="${record#*"$separator"}"
  case "$key" in
    *.helper) if [ -n "$value" ] && [ "$value" != ` + remote.Quote(credentialHelper) + ` ]; then dirty "global git config names a credential helper other than the provider's"; fi ;;
    *.usehttppath) ;;
    *) dirty "global git config sets a credential key other than helper or useHttpPath" ;;
  esac
done < "$lines"
rm -f "$records" "$lines"
if git -C "$root" config --local --get-regexp '^credential\.' > /dev/null; then dirty "$root/.git/config holds a credential setting"; fi
origin="$(git -C "$root" remote get-url origin)"
if [ "${origin%.git}" != ` + remote.Quote(strings.TrimSuffix(repository, ".git")) + ` ]; then dirty "origin is not ` + repository + `"; fi
set --
for path in ` + strings.Join(tokenScan, " ") + `; do
  if [ -e "$path" ]; then set -- "$@" "$path"; fi
done
scanned=0
tokens="$(grep -rIlE '` + tokenPattern + `' "$@")" || scanned=$?
if [ "$scanned" -gt 1 ]; then dirty "grep could not read every file it scanned for GitHub tokens"; fi
if [ -n "$tokens" ]; then dirty "a GitHub token is on disk in $tokens"; fi`
}

type KeyPair struct {
	Private string
	Public  string
}

func SSHKeyPair(ctx context.Context, dir, name string) (KeyPair, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return KeyPair{}, err
	}
	pair := KeyPair{Private: filepath.Join(dir, name), Public: filepath.Join(dir, name+".pub")}
	_, err := os.Stat(pair.Private)
	if err == nil {
		return pair, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return KeyPair{}, err
	}
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", remote.Prefix+" "+name, "-f", pair.Private)
	keygen.Stdout, keygen.Stderr = os.Stderr, os.Stderr
	if err := keygen.Run(); err != nil {
		return KeyPair{}, fmt.Errorf("generating the %s ssh key: %w", name, err)
	}
	return pair, nil
}

func (k KeyPair) PublicKey() ([]byte, error) {
	return os.ReadFile(k.Public)
}

func (k KeyPair) Remove() error {
	for _, path := range []string{k.Private, k.Public} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
