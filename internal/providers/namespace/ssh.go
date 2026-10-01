package namespace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-remote/internal/providers"
)

func Alias(id string) string { return id + ".devbox.namespace" }

func DefaultSSHDir(home string) string { return filepath.Join(home, ".namespace", "ssh") }

func (p *Provider) SSHTarget(ctx context.Context, id string) (providers.Target, error) {
	if _, err := p.find(ctx, id); err != nil {
		return providers.Target{}, err
	}
	path := filepath.Join(p.SSHDir, Alias(id)+".ssh")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := p.devbox(ctx, "configure-ssh", id); err != nil {
			return providers.Target{}, err
		}
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return providers.Target{}, err
	}
	options := sshConfig(raw)
	for _, key := range []string{"user", "identityfile", "proxycommand"} {
		if options[key] == "" {
			return providers.Target{}, fmt.Errorf("%s sets no %s", path, key)
		}
	}
	return providers.Target{
		Host:          Alias(id),
		Port:          22,
		User:          options["user"],
		IdentityFile:  options["identityfile"],
		ProxyCommand:  options["proxycommand"],
		HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyProxyTrusted},
	}, nil
}

func sshConfig(raw []byte) map[string]string {
	options := map[string]string{}
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(lines.Text()), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, quoted := strings.CutPrefix(value, `"`); quoted {
			value = strings.TrimSuffix(unquoted, `"`)
		}
		options[strings.ToLower(key)] = strings.ReplaceAll(value, "%%", "%")
	}
	return options
}
