package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	Name       = "sprites"
	DefaultCLI = "sprite"
	nameLimit  = 55
	user       = "sprite"
	pageSize   = 500
)

type Config struct {
	Org      string `yaml:"org"`
	CLI      string `yaml:"cli"`
	StateDir string `yaml:"-"`
	Helper   string `yaml:"-"`
}

func (c Config) validate() error {
	switch {
	case c.Org == "":
		return errors.New("sprites needs an org; set it in the sprites config")
	case c.CLI == "" || c.Helper == "" || !filepath.IsAbs(c.StateDir):
		return errors.New("sprites needs a cli, a helper binary, and an absolute state directory")
	}
	return nil
}

type Provider struct {
	Config
	Runner providers.Runner
}

var _ providers.Provider = (*Provider)(nil)

func New(config Config) (*Provider, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Provider{Config: config, Runner: providers.OSRunner{}}, nil
}

func (p *Provider) Traits() providers.Traits {
	return providers.Traits{
		TailnetMode: providers.TailnetKernel,
		Supervisor:  providers.SupervisorSpriteEnv,
		HostKeys:    true,
	}
}

func (p *Provider) command(stdin io.Reader, args ...string) providers.Command {
	return providers.Command{Name: p.CLI, Args: args, Stdin: stdin}
}

func (p *Provider) records() providers.Records {
	return providers.Records{Dir: filepath.Join(p.StateDir, Name, "machines")}
}

func (p *Provider) Check(ctx context.Context) error {
	status, body, err := p.api(ctx, "/v1/sprites?max_results=1")
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("sprite CLI %q not found; install it from https://sprites.dev", p.CLI)
	case err != nil:
		return fmt.Errorf("sprites org %s is not reachable; run 'sprite login -o %s': %w", p.Org, p.Org, err)
	case status == 401 || status == 403:
		return fmt.Errorf("not logged in to sprites org %s; run 'sprite login -o %s'", p.Org, p.Org)
	case status != 200:
		return fmt.Errorf("sprites api answered %d: %s", status, bytes.TrimSpace(body))
	}
	return nil
}

func (p *Provider) ValidateSpec(spec providers.Spec) error {
	switch {
	case spec.Image != "":
		return fmt.Errorf("a sprite boots no image; provision %s after create instead of naming %q", spec.Name, spec.Image)
	case spec.Size != "":
		return fmt.Errorf("sprites have one size, not %q", spec.Size)
	case spec.Region != "":
		return fmt.Errorf("sprites choose no region, not %q", spec.Region)
	}
	return nil
}

func (p *Provider) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if err := providers.CheckName(spec.Name, nameLimit); err != nil {
		return providers.Machine{}, err
	}
	if err := p.ValidateSpec(spec); err != nil {
		return providers.Machine{}, err
	}
	switch _, err := p.Get(ctx, spec.Name); {
	case err == nil:
		return providers.Machine{}, fmt.Errorf("sprite %s: %w", spec.Name, providers.ErrExists)
	case !errors.Is(err, providers.ErrNotFound):
		return providers.Machine{}, err
	}
	if _, err := providers.Output(ctx, p.Runner, p.command(nil, "create", "-o", p.Org, "--skip-console", spec.Name)); err != nil {
		if _, found := p.Get(ctx, spec.Name); found == nil {
			return providers.Machine{}, fmt.Errorf("sprite %s: %w: %w", spec.Name, providers.ErrAmbiguous, err)
		}
		return providers.Machine{}, fmt.Errorf("sprite %s: whether the create allocated it is unknown: %w", spec.Name, err)
	}
	created, err := p.Get(ctx, spec.Name)
	if err != nil {
		return providers.Machine{}, fmt.Errorf("sprite %s: %w: created but unreadable: %w", spec.Name, providers.ErrAmbiguous, err)
	}
	if err := p.records().Save(spec.Name, spec.Labels, created.CreatedAt); err != nil {
		return providers.Machine{}, fmt.Errorf("sprite %s: %w: created but its labels were not recorded: %w", spec.Name, providers.ErrAmbiguous, err)
	}
	created.Labels = maps.Clone(spec.Labels)
	return created, nil
}

type sprite struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type page struct {
	Sprites               []sprite `json:"sprites"`
	HasMore               bool     `json:"has_more"`
	NextContinuationToken string   `json:"next_continuation_token"`
}

func (p *Provider) machine(s sprite) (providers.Machine, error) {
	var state providers.State
	switch s.Status {
	case "running":
		state = providers.StateRunning
	case "warm", "cold":
		state = providers.StateSuspended
	default:
		return providers.Machine{}, fmt.Errorf("sprite %s has status %q", s.Name, s.Status)
	}
	labels, err := p.records().Labels(s.Name, s.CreatedAt)
	if err != nil {
		return providers.Machine{}, err
	}
	return providers.Machine{ID: s.Name, Provider: Name, State: state, CreatedAt: s.CreatedAt, Labels: labels}, nil
}

func (p *Provider) Get(ctx context.Context, id string) (providers.Machine, error) {
	if providers.CheckName(id, nameLimit) != nil {
		return providers.Machine{}, fmt.Errorf("sprite %q: %w", id, providers.ErrNotFound)
	}
	status, body, err := p.api(ctx, "/v1/sprites/"+id)
	if err != nil {
		return providers.Machine{}, err
	}
	switch status {
	case 200:
	case 404:
		return providers.Machine{}, fmt.Errorf("sprite %s: %w", id, providers.ErrNotFound)
	default:
		return providers.Machine{}, fmt.Errorf("sprites api answered %d for %s: %s", status, id, bytes.TrimSpace(body))
	}
	var found sprite
	if err := json.Unmarshal(body, &found); err != nil {
		return providers.Machine{}, fmt.Errorf("reading sprite %s: %w", id, err)
	}
	return p.machine(found)
}

func (p *Provider) List(ctx context.Context, labels map[string]string) ([]providers.Machine, error) {
	var machines []providers.Machine
	token := ""
	for {
		query := url.Values{"max_results": {strconv.Itoa(pageSize)}}
		if token != "" {
			query.Set("continuation_token", token)
		}
		status, body, err := p.api(ctx, "/v1/sprites?"+query.Encode())
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("sprites api answered %d listing sprites: %s", status, bytes.TrimSpace(body))
		}
		var listed page
		if err := json.Unmarshal(body, &listed); err != nil {
			return nil, fmt.Errorf("reading the sprite list: %w", err)
		}
		for _, s := range listed.Sprites {
			machine, err := p.machine(s)
			if err != nil {
				return nil, err
			}
			if machine.HasLabels(labels) {
				machines = append(machines, machine)
			}
		}
		if !listed.HasMore {
			return machines, nil
		}
		token = listed.NextContinuationToken
	}
}

func (p *Provider) Wake(ctx context.Context, id string) error {
	result, err := p.Exec(ctx, id, []string{"true"}, nil)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("waking sprite %s: exit %d: %s", id, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	return nil
}

// Sprites has no stop verb; a sprite with no session or request sleeps on its own.
func (p *Provider) Suspend(ctx context.Context, id string) error {
	_, err := p.Get(ctx, id)
	return err
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	if _, err := p.Get(ctx, id); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return errors.Join(err, p.keys().remove(id), p.records().Remove(id))
		}
		return err
	}
	if _, err := providers.Output(ctx, p.Runner, p.command(nil, "destroy", "-o", p.Org, "-s", id, "--force")); err != nil {
		return err
	}
	switch _, err := p.Get(ctx, id); {
	case errors.Is(err, providers.ErrNotFound):
		return errors.Join(p.keys().remove(id), p.records().Remove(id))
	case err != nil:
		return err
	}
	return nil
}

func (p *Provider) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	if _, err := p.Get(ctx, id); err != nil {
		return providers.Result{}, err
	}
	args := []string{"exec", "-o", p.Org, "-s", id, "--no-port-forward"}
	if stdin == nil {
		args = append(args, "--no-stdin")
	}
	return p.Runner.Run(ctx, p.command(stdin, append(append(args, "--"), cmd...)...))
}

func (p *Provider) api(ctx context.Context, path string) (int, []byte, error) {
	out, err := providers.Output(ctx, p.Runner, p.command(nil, "api", "-o", p.Org, path, "--", "-sS", "-w", "\n%{http_code}"))
	if err != nil {
		return 0, nil, err
	}
	cut := bytes.LastIndexByte(out, '\n')
	if cut < 0 {
		return 0, nil, fmt.Errorf("sprite api %s printed no status: %q", path, out)
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(out[cut+1:])))
	if err != nil {
		return 0, nil, fmt.Errorf("sprite api %s printed no status: %q", path, out)
	}
	return status, out[:cut], nil
}
