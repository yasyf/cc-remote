package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

const (
	devbox    = "devbox"
	waitDelay = 2 * time.Second
)

type Devbox interface {
	Run(ctx context.Context, args ...string) error
}

type DevboxCLI struct {
	Stdout io.Writer
	Stderr io.Writer
}

func (d DevboxCLI) Run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, devbox, args...)
	cmd.WaitDelay = waitDelay
	cmd.Stdout, cmd.Stderr = d.Stdout, d.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("devbox %s: %w", args[0], err)
	}
	return nil
}

func (c Context) Build(ctx context.Context, cli Devbox) (err error) {
	dir, err := os.MkdirTemp("", "cc-remote-image-")
	if err != nil {
		return fmt.Errorf("create image context: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := c.Write(dir); err != nil {
		return err
	}
	return cli.Run(ctx, c.BuildArgs(dir)...)
}

func (c Context) BuildArgs(dir string) []string {
	return []string{
		"image", "build", dir,
		"--name", c.Image.Name,
		"--description", "cc-remote agent host cc-remote-image=" + c.Fingerprint(),
		"--user", c.Image.User,
		"--on_startup", StartPath,
		"--workspace_dir", c.Image.WorkspaceDir,
		"--persistency", "whole",
	}
}
