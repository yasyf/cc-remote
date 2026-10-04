package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	NameLimit = 55
	dirName   = "cc-remote"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type Dir string

func Default() Dir {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return Dir(filepath.Join(root, dirName))
}

func ValidateName(name string) error {
	if !validName.MatchString(name) || len(name) > NameLimit {
		return fmt.Errorf("name %q: use up to %d lowercase letters, digits and dashes, starting with a letter or digit", name, NameLimit)
	}
	return nil
}

func (d Dir) Workspace(name string) string {
	return filepath.Join(string(d), "workspaces", name+".json")
}
func (d Dir) Tailnet() string { return filepath.Join(string(d), "tailnet") }
func (d Dir) SSH(name string) string {
	return filepath.Join(string(d), "ssh", name+".ssh")
}
func (d Dir) SSHInclude() string { return filepath.Join(string(d), "ssh", "*.ssh") }
func (d Dir) Orca(name string) string {
	return filepath.Join(string(d), "orca", name+".json")
}

func NewOrcaControl() (string, error) {
	dir, err := os.MkdirTemp("/tmp", "ccr-")
	if err != nil {
		return "", fmt.Errorf("create private SSH control directory: %w", err)
	}
	return filepath.Join(dir, "%C"), nil
}

func EnsureOrcaControl(control string) error {
	dir := filepath.Dir(control)
	if control != filepath.Join(dir, "%C") || filepath.Dir(dir) != "/tmp" || !strings.HasPrefix(filepath.Base(dir), "ccr-") {
		return fmt.Errorf("invalid recorded SSH control path %q", control)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("restore SSH control directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat SSH control directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return fmt.Errorf("SSH control directory %s must be a private directory owned by the current user", dir)
	}
	return nil
}

func (d Dir) OrcaForwardLog(name string) string {
	return filepath.Join(string(d), "orca", name+".forward.log")
}

func (d Dir) OrcaGatewayLock(name, instance string) string {
	return filepath.Join(string(d), "orca", name+"."+instance+".gateway.lock")
}

func (d Dir) OrcaLease(name, instance string) string {
	return filepath.Join(string(d), "orca", name+"."+instance+".lease.json")
}

func (d Dir) Pool(name string) string {
	return filepath.Join(string(d), "pool", name+".json")
}

func (d Dir) PoolFill(key string) string {
	return filepath.Join(string(d), "pool", "fill-"+key+".lock")
}

type Held struct {
	Name   string
	unlock func()
}

func (d Dir) Hold(name string) (*Held, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	unlock, err := Lock(d.Workspace(name) + ".lock")
	if err != nil {
		return nil, err
	}
	return &Held{Name: name, unlock: unlock}, nil
}

func (h *Held) Release() {
	h.unlock()
}

func Lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return func() { _ = file.Close() }, nil
}

func TryLock(path string) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	switch err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return nil, false, file.Close()
	case err != nil:
		return nil, false, errors.Join(err, file.Close())
	}
	return func() { _ = file.Close() }, true, nil
}

func Save(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Write(path, append(raw, '\n'))
}

func Write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func Load(path string, into any) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func Remove(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}
