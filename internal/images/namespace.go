package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	devbox    = "devbox"
	nsc       = "nsc"
	secretID  = "github-token"
	waitDelay = 2 * time.Second
)

var (
	imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	registryURL = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$`)
)

type Namespace interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type NamespaceCLI struct {
	Stderr io.Writer
	Region string
}

type Built struct {
	Profile     string          `json:"profile"`
	Platform    string          `json:"platform"`
	Fingerprint string          `json:"fingerprint"`
	Repository  string          `json:"repository"`
	Tag         string          `json:"tag"`
	Digest      string          `json:"digest"`
	Reference   string          `json:"reference"`
	Manifest    json.RawMessage `json:"manifest"`
}

func (n NamespaceCLI) Argv(name string, args []string) []string {
	if name == nsc && n.Region != "" {
		return append([]string{"--region", n.Region}, args...)
	}
	return args
}

func (n NamespaceCLI) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, n.Argv(name, args)...)
	cmd.WaitDelay = waitDelay
	cmd.Stdout, cmd.Stderr = n.Stderr, n.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

func (n NamespaceCLI) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, n.Argv(name, args)...)
	cmd.WaitDelay = waitDelay
	cmd.Stderr = n.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args[:min(2, len(args))], " "), err)
	}
	return out, nil
}

func (c Context) Repository() string { return c.Image.Name + "-" + c.Profile }

func (c Context) Build(ctx context.Context, cli Namespace, token func(context.Context) (string, error)) (built Built, err error) {
	dir, err := os.MkdirTemp("", "cc-remote-image-")
	if err != nil {
		return Built{}, fmt.Errorf("create image context: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := c.Write(dir); err != nil {
		return Built{}, err
	}
	if err := c.push(ctx, cli, dir, token); err != nil {
		return Built{}, err
	}
	described, err := cli.Output(ctx, nsc, c.DescribeArgs()...)
	if err != nil {
		return Built{}, err
	}
	digest, err := c.parseDescribed(described)
	if err != nil {
		return Built{}, err
	}
	workspace, err := cli.Output(ctx, nsc, "workspace", "describe", "--key", "registry_url", "--output", "json")
	if err != nil {
		return Built{}, err
	}
	registry, err := parseRegistry(workspace)
	if err != nil {
		return Built{}, err
	}
	reference := registry + "/" + c.Repository() + "@" + digest
	if err := cli.Run(ctx, devbox, c.WireArgs(reference)...); err != nil {
		return Built{}, err
	}
	return Built{
		Profile:     c.Profile,
		Platform:    c.Platform,
		Fingerprint: c.Fingerprint(),
		Repository:  c.Repository(),
		Tag:         c.Fingerprint(),
		Digest:      digest,
		Reference:   reference,
		Manifest:    json.RawMessage(c.Manifest),
	}, nil
}

func (c Context) push(ctx context.Context, cli Namespace, dir string, token func(context.Context) (string, error)) (err error) {
	secret := ""
	if c.Private {
		if secret, err = writeSecret(ctx, token); err != nil {
			return err
		}
		defer func() { err = errors.Join(err, os.Remove(secret)) }()
	}
	return cli.Run(ctx, nsc, c.BuildArgs(dir, secret)...)
}

func writeSecret(ctx context.Context, token func(context.Context) (string, error)) (string, error) {
	value, err := token(ctx)
	if err != nil {
		return "", fmt.Errorf("a private marketplace needs the GitHub token: %w", err)
	}
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("the GitHub token is empty or spans lines")
	}
	file, err := os.CreateTemp("", "cc-remote-github-token-")
	if err != nil {
		return "", fmt.Errorf("create the build secret: %w", err)
	}
	_, err = file.WriteString(value + "\n")
	if err = errors.Join(err, file.Close()); err != nil {
		return "", errors.Join(fmt.Errorf("write the build secret: %w", err), os.Remove(file.Name()))
	}
	return file.Name(), nil
}

func (c Context) BuildArgs(dir, secret string) []string {
	args := []string{"build", dir, "--file", "Dockerfile", "--platform", c.Platform, "--name", c.Repository() + ":" + c.Fingerprint(), "--push"}
	if secret != "" {
		args = append(args, "--secret", "id="+secretID+",src="+secret)
	}
	return args
}

func (c Context) DescribeArgs() []string {
	return []string{"registry", "describe", "--repository", c.Repository(), "--reference", c.Fingerprint(), "--output", "json"}
}

func (c Context) WireArgs(reference string) []string {
	return []string{
		"image", "wire", reference,
		"--description", "cc-remote agent host cc-remote-image=" + c.Fingerprint(),
		"--user", c.Image.User,
		"--on_startup", StartPath,
		"--workspace_dir", c.Image.WorkspaceDir,
		"--persistency", "whole",
	}
}

func (c Context) parseDescribed(out []byte) (string, error) {
	var described struct {
		Image struct {
			Repository string `json:"repository"`
			Digest     string `json:"digest"`
		} `json:"image"`
	}
	if err := json.Unmarshal(out, &described); err != nil {
		return "", fmt.Errorf("decode nsc registry describe: %w", err)
	}
	if described.Image.Repository != c.Repository() || !imageDigest.MatchString(described.Image.Digest) {
		return "", fmt.Errorf("nsc registry describe named %s@%s, not a digest of %s", described.Image.Repository, described.Image.Digest, c.Repository())
	}
	return described.Image.Digest, nil
}

func parseRegistry(out []byte) (string, error) {
	registry := strings.TrimSpace(string(out))
	if !registryURL.MatchString(registry) {
		return "", fmt.Errorf("nsc workspace describe printed %q, not one registry URL line", registry)
	}
	return registry, nil
}
