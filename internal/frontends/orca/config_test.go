package orca_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func exampleConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSourceOf(t *testing.T) {
	want := orca.Source{
		Provider:  "sprites",
		Profile:   "lean",
		Providers: []string{"namespace", "sprites"},
		Profiles:  []string{"full", "lean"},
	}
	if got := orca.SourceOf(exampleConfig(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("SourceOf() = %+v, want %+v", got, want)
	}
}

func TestPluginOf(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		version    string
		want       orca.Plugin
		wantErr    string
	}{
		{
			name:       "github repository and a release version",
			repository: "https://github.com/Example-Org/My_Project.git",
			version:    "v1.4.0",
			want: orca.Plugin{
				ID:          "cc-remote-recipes",
				Publisher:   "example-org",
				Name:        "my-project remote workspaces",
				Version:     "1.4.0",
				Description: "Remote workspaces for my-project, created by cc-remote over SSH.",
				Repository:  "https://github.com/Example-Org/My_Project.git",
			},
		},
		{
			name:       "local clone and a dev build",
			repository: "file:///srv/git/project",
			version:    "dev",
			want: orca.Plugin{
				ID:          "cc-remote-recipes",
				Publisher:   "local",
				Name:        "project remote workspaces",
				Version:     "0.0.0-dev",
				Description: "Remote workspaces for project, created by cc-remote over SSH.",
				Repository:  "file:///srv/git/project",
			},
		},
		{name: "no owner", repository: "https://example.com", version: "v1.0.0", wantErr: "names no owner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := exampleConfig(t)
			cfg.Repository = tt.repository
			got, err := orca.PluginOf(cfg, tt.version)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("PluginOf() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("PluginOf() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}
