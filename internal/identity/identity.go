package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/yasyf/cc-remote/internal/remote"
)

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
