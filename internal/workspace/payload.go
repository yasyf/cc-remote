package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	LabelPayloadBuild = "cc-remote/payload-build"
	payloadPrefix     = "cc-remote-payload-"
)

type Downloader interface {
	Download(ctx context.Context, id, path string, w io.Writer) error
}

type PayloadBuild struct {
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Tools   string `json:"tools"`
	Machine string `json:"machine"`
}

type byteCount int64

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

func GitToken(cfg *config.Config) func(context.Context) (string, error) {
	return (&Session{Config: cfg}).gitToken
}

func BuildPayload(ctx context.Context, cfg *config.Config, provider providers.Provider, kind, profile string, token func(context.Context) (string, error), out, stderr io.Writer) (PayloadBuild, error) {
	downloader, ok := provider.(Downloader)
	if !ok {
		return PayloadBuild{}, fmt.Errorf("the %s provider cannot download a file from a machine, so it cannot build a payload", kind)
	}
	spec, err := cfg.ProfileNamed(profile)
	if err != nil {
		return PayloadBuild{}, err
	}
	machine := spec.Machine[kind]
	if machine.Image != "" {
		return PayloadBuild{}, fmt.Errorf("profile %s boots %s machines from image %q; a payload is built on a machine provisioned in place", profile, kind, machine.Image)
	}
	rendered, err := render(cfg, profile, false)
	if err != nil {
		return PayloadBuild{}, err
	}
	var githubToken string
	if rendered.private {
		if githubToken, err = token(ctx); err != nil {
			return PayloadBuild{}, err
		}
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := payloadPrefix + hex.EncodeToString(suffix)
	build := providers.Spec{Name: name, Profile: profile, Size: machine.Size, Region: machine.Region, Labels: map[string]string{LabelPayloadBuild: name}}
	if _, err := provider.Create(ctx, build); err != nil {
		if errors.Is(err, providers.ErrExists) {
			return PayloadBuild{}, fmt.Errorf("payload build machine %s already exists at the provider, so it was left alone: %w", name, err)
		}
		return PayloadBuild{}, errors.Join(err, discardPayloadBuild(context.WithoutCancel(ctx), provider, name))
	}
	slog.Info("created the payload build machine", "machine", name)
	builder := &Session{Provider: provider, Stderr: stderr}
	built, err := packPayload(ctx, builder.exec(name), downloader, rendered.scripts, githubToken, name, out)
	if err := errors.Join(err, discardPayloadBuild(context.WithoutCancel(ctx), provider, name)); err != nil {
		return PayloadBuild{}, err
	}
	return built, nil
}

func packPayload(ctx context.Context, run images.Exec, downloader Downloader, scripts images.Scripts, githubToken, machine string, out io.Writer) (PayloadBuild, error) {
	tools := scripts.Fingerprint()
	steps := []func() error{
		func() error { return scripts.Provision(ctx, run, images.PhasePackages) },
		func() error { return scripts.Provision(ctx, run, images.PhaseTools) },
		func() error { return scripts.StagePlugins(ctx, run) },
		func() error { return scripts.Install(ctx, run, githubToken, "") },
		func() error { return scripts.Provision(ctx, run, images.PhasePack, tools) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return PayloadBuild{}, err
		}
	}
	slog.Info("packed the payload", "machine", machine, "tools", tools[:12])
	digest := sha256.New()
	var size byteCount
	if err := downloader.Download(ctx, machine, images.PackPath, io.MultiWriter(out, digest, &size)); err != nil {
		return PayloadBuild{}, err
	}
	return PayloadBuild{SHA256: hex.EncodeToString(digest.Sum(nil)), Size: int64(size), Tools: tools, Machine: machine}, nil
}

func discardPayloadBuild(ctx context.Context, provider providers.Provider, machine string) error {
	found, err := provider.Get(ctx, machine)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("could not tell whether payload build machine %s is still there, so check the provider for it: %w", machine, err)
	case found.Labels[LabelPayloadBuild] != machine:
		return fmt.Errorf("payload build machine %s exists at the provider without a label proving this build made it (want %s=%s, labels %v), so it was left running: if it is yours, remove it at the provider", machine, LabelPayloadBuild, machine, found.Labels)
	}
	if err := provider.Destroy(ctx, machine); err != nil && !errors.Is(err, providers.ErrNotFound) {
		return fmt.Errorf("removing payload build machine %s failed, so check the provider for it: %w", machine, err)
	}
	switch _, err := provider.Get(ctx, machine); {
	case errors.Is(err, providers.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("could not confirm payload build machine %s is gone, so check the provider for it: %w", machine, err)
	}
	return fmt.Errorf("payload build machine %s is still at the provider after its destroy; remove it there", machine)
}
