package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Records struct {
	Dir string
}

type record struct {
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"created"`
}

func (r Records) path(id string) string { return filepath.Join(r.Dir, id+".json") }

func (r Records) Labels(id string, created time.Time) (map[string]string, error) {
	raw, err := os.ReadFile(r.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved record
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path(id), err)
	}
	if !saved.Created.Equal(created) {
		return nil, nil
	}
	return saved.Labels, nil
}

func (r Records) Save(id string, labels map[string]string, created time.Time) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(record{Labels: labels, Created: created})
	if err != nil {
		return err
	}
	return WriteFileAtomic(r.path(id), raw, 0o600)
}

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, writeErr := staged.Write(data)
	if err := errors.Join(writeErr, staged.Chmod(perm), staged.Close()); err != nil {
		return errors.Join(err, os.Remove(staged.Name()))
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		return errors.Join(err, os.Remove(staged.Name()))
	}
	return nil
}

func (r Records) Remove(id string) error {
	if err := os.Remove(r.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

var machineName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func CheckName(name string, limit int) error {
	if len(name) > limit || !machineName.MatchString(name) {
		return fmt.Errorf("machine name %q must be at most %d lowercase letters, digits, and inner hyphens", name, limit)
	}
	return nil
}

var bare = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func ShellQuote(words ...string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		if bare.MatchString(word) {
			quoted = append(quoted, word)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}
