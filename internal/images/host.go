package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

type Exec func(ctx context.Context, argv []string, stdin io.Reader) error

const stagePlugins = `set -eu
mkdir -p "$HOME/.cc-remote"
cat > "` + PluginsPath + `.tmp"
mv -f "` + PluginsPath + `.tmp" "` + PluginsPath + `"`

func (s Scripts) ProvisionInPlace(ctx context.Context, exec Exec) error {
	if err := exec(ctx, []string{"sudo", "bash", "-s"}, bytes.NewReader(s.Provision)); err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	return nil
}

func (s Scripts) StagePlugins(ctx context.Context, exec Exec) error {
	if err := exec(ctx, []string{"sh", "-c", stagePlugins}, bytes.NewReader(s.Plugins)); err != nil {
		return fmt.Errorf("stage plugins.sh: %w", err)
	}
	return nil
}

func (s Scripts) Install(ctx context.Context, exec Exec, githubToken string) error {
	if strings.ContainsAny(githubToken, "\r\n") {
		return errors.New("plugins install: the GitHub token spans lines")
	}
	return runPlugins(ctx, exec, "install", nil, strings.NewReader(githubToken+"\n"))
}

func (s Scripts) Configure(ctx context.Context, exec Exec, env map[string]string) error {
	if have, want := slices.Sorted(maps.Keys(env)), slices.Sorted(slices.Values(s.Env)); !slices.Equal(have, want) {
		return fmt.Errorf("plugins configure: got environment %v, the inventory declares %v", have, want)
	}
	assignments := make([]string, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		assignments = append(assignments, key+"="+env[key])
	}
	return runPlugins(ctx, exec, "configure", assignments, nil)
}

func (s Scripts) Verify(ctx context.Context, exec Exec) error {
	return runPlugins(ctx, exec, "verify", nil, nil)
}

func runPlugins(ctx context.Context, exec Exec, phase string, env []string, stdin io.Reader) error {
	argv := slices.Concat([]string{"env"}, env, []string{"bash", "-c", `exec bash "` + PluginsPath + `" "$0"`, phase})
	if err := exec(ctx, argv, stdin); err != nil {
		return fmt.Errorf("plugins %s: %w", phase, err)
	}
	return nil
}
