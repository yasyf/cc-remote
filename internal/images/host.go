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

const (
	PayloadRoot  = "/opt/cc-remote/payload"
	PayloadStore = "/var/lib/cc-remote/payload"
	PackPath     = "/var/lib/cc-remote/build/payload.sqfs"
)

const (
	PhasePrerequisites = "prerequisites"
	PhasePackages      = "packages"
	PhaseTools         = "tools"
	PhasePayload       = "payload"
	PhasePack          = "pack"
)

const stagePlugins = `set -eu
mkdir -p "$HOME/.cc-remote"
cat > "` + PluginsPath + `.tmp"
mv -f "` + PluginsPath + `.tmp" "` + PluginsPath + `"`

const stagePayload = "install -d -m 0755 " + PayloadStore + " && cat > " + PayloadStore + "/$1.sqfs.partial"

func (s Scripts) Provision(ctx context.Context, exec Exec, phase string, args ...string) error {
	argv := slices.Concat([]string{"sudo", "bash", "-s", phase}, args)
	if err := exec(ctx, argv, bytes.NewReader(s.ProvisionScript)); err != nil {
		return fmt.Errorf("provision %s: %w", phase, err)
	}
	return nil
}

func (s Scripts) StagePayload(ctx context.Context, exec Exec, image io.Reader, digest string) error {
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("stage payload: %q is not a sha256 digest", digest)
	}
	if err := exec(ctx, []string{"sudo", "sh", "-c", stagePayload, "stage-payload", digest}, image); err != nil {
		return fmt.Errorf("stage payload %s: %w", digest, err)
	}
	return nil
}

func (s Scripts) StagePlugins(ctx context.Context, exec Exec) error {
	if err := exec(ctx, []string{"sh", "-c", stagePlugins}, bytes.NewReader(s.Plugins)); err != nil {
		return fmt.Errorf("stage plugins.sh: %w", err)
	}
	return nil
}

func (s Scripts) Install(ctx context.Context, exec Exec, githubToken, payload string) error {
	if strings.ContainsAny(githubToken, "\r\n") {
		return errors.New("plugins install: the GitHub token spans lines")
	}
	args := []string{"install"}
	if payload != "" {
		if !sha256Pattern.MatchString(payload) {
			return fmt.Errorf("plugins install: payload %q is not a sha256 digest", payload)
		}
		args = append(args, PayloadRoot+"/"+payload)
	}
	return runPlugins(ctx, exec, args, nil, strings.NewReader(githubToken+"\n"))
}

func (s Scripts) Publish(ctx context.Context, exec Exec, stamp string) error {
	if !sha256Pattern.MatchString(stamp) {
		return fmt.Errorf("plugins publish: stamp %q is not a fingerprint", stamp)
	}
	return runPlugins(ctx, exec, []string{"publish", stamp}, nil, nil)
}

func (s Scripts) Ready(ctx context.Context, exec Exec, stamp string) error {
	if !sha256Pattern.MatchString(stamp) {
		return fmt.Errorf("plugins ready: stamp %q is not a fingerprint", stamp)
	}
	return runPlugins(ctx, exec, []string{"ready", stamp}, nil, nil)
}

func (s Scripts) Configure(ctx context.Context, exec Exec, env map[string]string) error {
	if have, want := slices.Sorted(maps.Keys(env)), slices.Sorted(slices.Values(s.Env)); !slices.Equal(have, want) {
		return fmt.Errorf("plugins configure: got environment %v, the inventory declares %v", have, want)
	}
	assignments := make([]string, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		assignments = append(assignments, key+"="+env[key])
	}
	return runPlugins(ctx, exec, []string{"configure"}, assignments, nil)
}

func (s Scripts) Verify(ctx context.Context, exec Exec) error {
	return runPlugins(ctx, exec, []string{"verify"}, nil, nil)
}

func runPlugins(ctx context.Context, exec Exec, args, env []string, stdin io.Reader) error {
	argv := slices.Concat([]string{"env"}, env, []string{"bash", "-c", `exec bash "` + PluginsPath + `" "$@"`, "plugins.sh"}, args)
	if err := exec(ctx, argv, stdin); err != nil {
		return fmt.Errorf("plugins %s: %w", args[0], err)
	}
	return nil
}
