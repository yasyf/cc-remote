package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yasyf/cc-remote/internal/state"
)

const (
	EnvPath                = "CC_REMOTE_CONFIG"
	DefaultTailnetAPI      = "https://api.tailscale.com/api/v2"
	DefaultKeychainService = "cc-remote-tailnet"
	defaultLedger          = "default"
	dirName                = "cc-remote"
	fileName               = "config.yaml"
)

type Checkout string

const (
	Shallow Checkout = "shallow"
	Full    Checkout = "full"
)

type Config struct {
	Repository string                    `yaml:"repository"`
	Ref        string                    `yaml:"ref"`
	Provider   string                    `yaml:"provider"`
	Profile    string                    `yaml:"profile"`
	StateDir   string                    `yaml:"state_dir"`
	Providers  map[string]yaml.Node      `yaml:"providers"`
	Roots      map[string]string         `yaml:"workspace_dirs"`
	Profiles   map[string]Profile        `yaml:"profiles"`
	Spares     map[string]map[string]int `yaml:"spares"`
	Bootstrap  string                    `yaml:"bootstrap"`
	Forwards   []Forward                 `yaml:"forwards"`
	Budget     Budget                    `yaml:"budget"`
	Tailnet    *Tailnet                  `yaml:"tailnet"`
	Git        Git                       `yaml:"git"`
	Identity   Identity                  `yaml:"identity"`

	Path string `yaml:"-"`
}

type Profile struct {
	Checkout   Checkout           `yaml:"checkout"`
	Prepare    []string           `yaml:"prepare"`
	Warm       []string           `yaml:"warm"`
	WarmInputs []string           `yaml:"warm_inputs"`
	Machine    map[string]Machine `yaml:"machine"`
}

type Machine struct {
	Image  string `yaml:"image"`
	Size   string `yaml:"size"`
	Region string `yaml:"region"`
}

type Forward struct {
	Label string `yaml:"label"`
	Env   string `yaml:"env"`
}

type Budget struct {
	Ledger     string  `yaml:"ledger"`
	CapUSD     float64 `yaml:"cap_usd"`
	ReserveUSD float64 `yaml:"reserve_usd"`
	TrialHours float64 `yaml:"trial_hours"`
}

type Tailnet struct {
	Tag             string `yaml:"tag"`
	KeychainService string `yaml:"keychain_service"`
	API             string `yaml:"api"`
}

type Git struct {
	TokenCommand []string `yaml:"token_command"`
}

type Identity struct {
	ForbiddenPaths []string `yaml:"forbidden_paths"`
}

var (
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	ledgerName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)
)

func DefaultPath() string {
	if path := os.Getenv(EnvPath); path != "" {
		return path
	}
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(root, dirName, fileName)
}

func Load(path string) (*Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(absolute)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absolute, err)
	}
	cfg.Path = absolute
	return cfg, nil
}

func Parse(raw []byte) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	cfg := &Config{}
	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Budget.Ledger == "" {
		c.Budget.Ledger = defaultLedger
	}
	if c.Tailnet != nil {
		if c.Tailnet.API == "" {
			c.Tailnet.API = DefaultTailnetAPI
		}
		if c.Tailnet.KeychainService == "" {
			c.Tailnet.KeychainService = DefaultKeychainService
		}
	}
	if len(c.Git.TokenCommand) == 0 {
		c.Git.TokenCommand = []string{"gh", "auth", "token"}
	}
	for name, profile := range c.Profiles {
		if profile.Checkout == "" {
			profile.Checkout = Shallow
			c.Profiles[name] = profile
		}
	}
}

func (c *Config) validate() error {
	if !strings.HasPrefix(c.Repository, "https://") && !strings.HasPrefix(c.Repository, "file://") {
		return fmt.Errorf("repository %q must be an https or file clone URL", c.Repository)
	}
	if c.Ref == "" {
		return errors.New("ref names the branch or tag a workspace checks out when none is requested")
	}
	if len(c.Providers) == 0 {
		return errors.New("providers lists no provider")
	}
	if len(c.Profiles) == 0 {
		return errors.New("profiles lists no profile")
	}
	if _, ok := c.Providers[c.Provider]; !ok {
		return fmt.Errorf("provider %q is not under providers", c.Provider)
	}
	if _, ok := c.Profiles[c.Profile]; !ok {
		return fmt.Errorf("profile %q is not under profiles", c.Profile)
	}
	for kind := range c.Providers {
		if root := c.Roots[kind]; !strings.HasPrefix(root, "/") {
			return fmt.Errorf("workspace_dirs.%s %q must be the absolute directory checkouts live under on that provider's machines", kind, root)
		}
	}
	for kind := range c.Roots {
		if _, ok := c.Providers[kind]; !ok {
			return fmt.Errorf("workspace_dirs names provider %q, which is not under providers", kind)
		}
	}
	for name, profile := range c.Profiles {
		if !profileName.MatchString(name) {
			return fmt.Errorf("profile %q: use up to 20 lowercase letters, digits and dashes; the name is part of every spare's machine name", name)
		}
		if profile.Checkout != Shallow && profile.Checkout != Full {
			return fmt.Errorf("profile %s: checkout %q is neither shallow nor full", name, profile.Checkout)
		}
		for kind := range profile.Machine {
			if _, ok := c.Providers[kind]; !ok {
				return fmt.Errorf("profile %s: machine names provider %q, which is not under providers", name, kind)
			}
		}
	}
	for kind, counts := range c.Spares {
		if _, ok := c.Providers[kind]; !ok {
			return fmt.Errorf("spares names provider %q, which is not under providers", kind)
		}
		for profile, count := range counts {
			if _, ok := c.Profiles[profile]; !ok {
				return fmt.Errorf("spares.%s names profile %q, which is not under profiles", kind, profile)
			}
			if count < 0 {
				return fmt.Errorf("spares.%s.%s is negative", kind, profile)
			}
		}
	}
	if !ledgerName.MatchString(c.Budget.Ledger) || c.Budget.CapUSD <= 0 || c.Budget.TrialHours <= 0 || c.Budget.ReserveUSD < 0 {
		return errors.New("budget needs a lowercase ledger name, a positive cap_usd, a positive trial_hours and a non-negative reserve_usd")
	}
	if c.Tailnet != nil && !strings.HasPrefix(c.Tailnet.Tag, "tag:") {
		return fmt.Errorf("tailnet.tag %q must name the tag workspace nodes join under, like tag:cc-remote", c.Tailnet.Tag)
	}
	labels := map[string]bool{}
	for _, forward := range c.Forwards {
		if forward.Label == "" || !envName.MatchString(forward.Env) || labels[forward.Label] {
			return fmt.Errorf("forward %+v needs a unique label and a shell variable name in env", forward)
		}
		labels[forward.Label] = true
	}
	for _, path := range c.Identity.ForbiddenPaths {
		if path == "" || strings.ContainsAny(path, "'\"`\n") || strings.Contains(path, "$(") {
			return fmt.Errorf("identity.forbidden_paths entry %q is not a plain path", path)
		}
	}
	return nil
}

func (c *Config) RawProviderSection(kind string) ([]byte, error) {
	node, ok := c.Providers[kind]
	if !ok {
		return nil, fmt.Errorf("provider %q is not under providers", kind)
	}
	return yaml.Marshal(&node)
}

func (c *Config) ProviderSection(kind string) (func(into any) error, error) {
	raw, err := c.RawProviderSection(kind)
	if err != nil {
		return nil, err
	}
	return func(into any) error {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(into); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("providers.%s: %w", kind, err)
		}
		return nil
	}, nil
}

func (c *Config) ProfileNamed(name string) (Profile, error) {
	profile, ok := c.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("config has no %s profile", name)
	}
	return profile, nil
}

func (c *Config) SpareCount(provider, profile string) int {
	return c.Spares[provider][profile]
}

func (c *Config) ProjectRoot(provider string) string {
	return c.Roots[provider] + "/" + RepositoryName(c.Repository)
}

func (c *Config) ScriptPath(relative string) string {
	if filepath.IsAbs(relative) {
		return relative
	}
	return filepath.Join(filepath.Dir(c.Path), relative)
}

func (c *Config) State() state.Dir {
	if c.StateDir == "" {
		return state.Default()
	}
	if strings.HasPrefix(c.StateDir, "~/") {
		return state.Dir(filepath.Join(os.Getenv("HOME"), c.StateDir[2:]))
	}
	return state.Dir(c.StateDir)
}

func RepositoryName(repository string) string {
	return strings.TrimSuffix(repository[strings.LastIndex(repository, "/")+1:], ".git")
}
