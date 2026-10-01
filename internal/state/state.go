package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

func (d Dir) Ledger(name string) string { return filepath.Join(string(d), "ledgers", name+".json") }

func (d Dir) Workspace(name string) string {
	return filepath.Join(string(d), "workspaces", name+".json")
}
func (d Dir) Tailnet() string { return filepath.Join(string(d), "tailnet") }
func (d Dir) SSH(name string) string {
	return filepath.Join(string(d), "ssh", name+".ssh")
}
func (d Dir) SSHInclude() string { return filepath.Join(string(d), "ssh", "*.ssh") }
func (d Dir) Log(kind, name string) string {
	return filepath.Join(string(d), "logs", kind, name+".log")
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
