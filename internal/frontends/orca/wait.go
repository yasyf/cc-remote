package orca

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	runtimeReady    = "ready"
	windowAvailable = "available"
	checkFailed     = "fail"
	checkWarned     = "warn"
	orcaYAMLFile    = "orca.yaml"
)

var (
	commandChecks    = []string{"recipe.create", "recipe.suspend", "recipe.resume", "recipe.destroy"}
	ErrNotReady      = errors.New("the Orca desktop app is not running with a ready runtime and a window")
	ErrMissingRecipe = errors.New("the recipe is missing from orca.yaml, the only file the Orca CLI doctor reads")
	ErrStaleRecipe   = errors.New("the orca.yaml recipe differs from the cc-remote config")
)

type Poll struct {
	Interval time.Duration
	Timeout  time.Duration
}

type Preflight struct {
	Recipe    Recipe
	Lifecycle Lifecycle
	Workspace string
	RepoID    string
	Checkout  string
	SSH       SSHConfig
}

type SSHConfig struct {
	Path    string
	Home    string
	Include string
}

type SSHWorkspace struct {
	Recipe    string `json:"recipe"`
	Name      string `json:"name"`
	Checkout  string `json:"checkout"`
	Workspace string `json:"workspace"`
	Worktree  string `json:"worktree"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
}

func (c Client) Wait(ctx context.Context, p Preflight, wait Poll) (SSHWorkspace, error) {
	repo, err := c.preflight(ctx, p)
	if err != nil {
		return SSHWorkspace{}, err
	}
	slog.Info("waiting for Orca to list the SSH workspace it creates from the recipe; create it from the New Workspace composer",
		"project", repo.DisplayName, "workspace", p.Workspace, "runOn", p.Recipe.Name, "timeout", wait.Timeout)
	row, err := poll(ctx, wait, func(ctx context.Context) (Worktree, bool, error) {
		rows, err := c.sshWorktrees(ctx, p.Workspace, repo.ID)
		if err != nil || len(rows) == 0 {
			return Worktree{}, false, err
		}
		return rows[0], true, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return SSHWorkspace{}, fmt.Errorf("no SSH workspace named %s after %s: %w", p.Workspace, wait.Timeout, err)
	}
	if err != nil {
		return SSHWorkspace{}, err
	}
	return SSHWorkspace{
		Recipe:    p.Recipe.ID(),
		Name:      p.Recipe.Name,
		Checkout:  repo.Path,
		Workspace: p.Workspace,
		Worktree:  row.ID,
		Host:      row.HostID,
		Path:      row.Path,
		Branch:    row.Branch,
	}, nil
}

func (c Client) preflight(ctx context.Context, p Preflight) (Repo, error) {
	status, err := c.Status(ctx)
	if err != nil {
		return Repo{}, err
	}
	if status.Runtime.State != runtimeReady || status.App.DesktopWindowStatus != windowAvailable {
		return Repo{}, ErrNotReady
	}
	repo, err := c.repo(ctx, p.RepoID, p.Checkout)
	if err != nil {
		return Repo{}, err
	}
	if err := checkOrcaYAML(filepath.Join(repo.Path, orcaYAMLFile), p.Lifecycle, p.Recipe); err != nil {
		return Repo{}, err
	}
	report, err := c.Doctor(ctx, p.Recipe.ID(), repo.Path)
	if err != nil {
		return Repo{}, err
	}
	if err := report.err(p.Lifecycle); err != nil {
		return Repo{}, err
	}
	if err := p.SSH.check(); err != nil {
		return Repo{}, err
	}
	return repo, nil
}

func (c Client) repo(ctx context.Context, id, checkout string) (Repo, error) {
	repos, err := c.Repos(ctx)
	if err != nil {
		return Repo{}, err
	}
	for _, r := range repos {
		if id != "" && r.ID == id || id == "" && filepath.Clean(r.Path) == filepath.Clean(checkout) {
			return r, nil
		}
	}
	if id != "" {
		return Repo{}, fmt.Errorf("no Orca repo has id %s", id)
	}
	return Repo{}, fmt.Errorf("no Orca repo at %s; add the checkout to Orca or pass --repo", checkout)
}

func (c Client) sshWorktrees(ctx context.Context, name, repoID string) ([]Worktree, error) {
	rows, err := c.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(w Worktree) bool {
		return w.DisplayName != name || !w.OnSSH() || repoID != "" && w.RepoID != repoID
	}), nil
}

func (r DoctorReport) err(l Lifecycle) error {
	var failed []string
	for _, check := range r.Checks {
		switch {
		case check.Status == checkWarned && l.expectedWarning(check):
			slog.Info("orca vm recipe doctor warns on every command outside the repo", "recipe", r.RecipeID, "check", check.ID, "message", check.Message)
		case check.Status == checkFailed, check.Status == checkWarned:
			failed = append(failed, fmt.Sprintf("doctor %s: %s: %s", check.Status, check.ID, check.Message))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("orca vm recipe doctor %s: %s", r.RecipeID, strings.Join(failed, "; "))
}

func (l Lifecycle) expectedWarning(check DoctorCheck) bool {
	return !strings.HasPrefix(l.Binary, "./") && slices.Contains(commandChecks, check.ID)
}

func checkOrcaYAML(path string, l Lifecycle, r Recipe) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	entries, err := ParseYAML(data)
	if err != nil {
		return err
	}
	want, err := Entries(l, []Recipe{r})
	if err != nil {
		return err
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.ID == r.ID() })
	switch {
	case i < 0:
		return fmt.Errorf("%s: %w; write it with `cc-remote orca recipes --orca-yaml %s`", r.ID(), ErrMissingRecipe, path)
	case entries[i] != want[0]:
		return fmt.Errorf("%s: %w; rewrite it with `cc-remote orca recipes --orca-yaml %s`", r.ID(), ErrStaleRecipe, path)
	}
	return nil
}

func (s SSHConfig) check() error {
	data, err := os.ReadFile(s.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.Path, err)
	}
	for line := range strings.Lines(string(data)) {
		switch keyword, args := sshDirective(line); keyword {
		case "include":
			if slices.ContainsFunc(args, func(arg string) bool { return s.expand(arg) == s.Include }) {
				return nil
			}
		case "host", "match":
			return s.missing()
		}
	}
	return s.missing()
}

func (s SSHConfig) expand(arg string) string {
	if rest, ok := strings.CutPrefix(arg, "~/"); ok {
		return filepath.Join(s.Home, rest)
	}
	return arg
}

func (s SSHConfig) missing() error {
	include := s.Include
	if strings.ContainsAny(include, " \t") {
		include = `"` + include + `"`
	}
	return fmt.Errorf("add `Include %s` to %s above every Host and Match block, so Orca resolves workspace hosts through the Host blocks cc-remote writes", include, s.Path)
}

func sshDirective(line string) (string, []string) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), nil
	}
	return strings.ToLower(line[:i]), sshArgs(strings.TrimPrefix(strings.TrimLeft(line[i:], " \t"), "="))
}

func sshArgs(s string) []string {
	var args []string
	for i := 0; i < len(s); {
		switch s[i] {
		case ' ', '\t':
			i++
			continue
		case '#':
			return args
		}
		var arg strings.Builder
		var quote byte
	token:
		for ; i < len(s); i++ {
			switch c := s[i]; {
			case c == '\\' && i+1 < len(s) && (strings.IndexByte(`'"\`, s[i+1]) >= 0 || quote == 0 && s[i+1] == ' '):
				i++
				arg.WriteByte(s[i])
			case quote == 0 && (c == ' ' || c == '\t'):
				break token
			case quote == 0 && (c == '"' || c == '\''):
				quote = c
			case quote != 0 && c == quote:
				quote = 0
			default:
				arg.WriteByte(c)
			}
		}
		if quote != 0 {
			return nil
		}
		args = append(args, arg.String())
	}
	return args
}

func poll[T any](ctx context.Context, p Poll, check func(context.Context) (T, bool, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		value, done, err := check(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return value, ctxErr
		}
		if err != nil || done {
			return value, err
		}
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-ticker.C:
		}
	}
}
