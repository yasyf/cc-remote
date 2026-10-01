package registry

import (
	"fmt"
	"os"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/namespace"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
)

type Host struct {
	StateDir string
	Home     string
	Helper   string
}

func CurrentHost(stateDir string) (Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Host{}, err
	}
	helper, err := os.Executable()
	if err != nil {
		return Host{}, err
	}
	return Host{StateDir: stateDir, Home: home, Helper: helper}, nil
}

func New(kind string, host Host, section func(into any) error) (providers.Provider, error) {
	switch kind {
	case sprites.Name:
		config := sprites.Config{CLI: sprites.DefaultCLI}
		if err := section(&config); err != nil {
			return nil, fmt.Errorf("providers.%s: %w", kind, err)
		}
		config.StateDir, config.Helper = host.StateDir, host.Helper
		provider, err := sprites.New(config)
		if err != nil {
			return nil, fmt.Errorf("providers.%s: %w", kind, err)
		}
		return provider, nil
	case namespace.Name:
		config := namespace.Config{CLI: namespace.DefaultCLI, SSHDir: namespace.DefaultSSHDir(host.Home)}
		if err := section(&config); err != nil {
			return nil, fmt.Errorf("providers.%s: %w", kind, err)
		}
		config.StateDir = host.StateDir
		provider, err := namespace.New(config)
		if err != nil {
			return nil, fmt.Errorf("providers.%s: %w", kind, err)
		}
		return provider, nil
	}
	return nil, fmt.Errorf("unknown provider kind %q; want %s or %s", kind, sprites.Name, namespace.Name)
}
