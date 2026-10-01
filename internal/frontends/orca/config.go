package orca

import (
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yasyf/cc-remote/internal/config"
)

const localPublisher = "local"

var (
	pluginSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	slugBreaks = regexp.MustCompile(`[^a-z0-9]+`)
	semver     = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
)

func SourceOf(cfg *config.Config) Source {
	return Source{
		Provider:  cfg.Provider,
		Profile:   cfg.Profile,
		Providers: slices.Sorted(maps.Keys(cfg.Providers)),
		Profiles:  slices.Sorted(maps.Keys(cfg.Profiles)),
	}
}

func SSHIncludeOf(cfg *config.Config) string {
	fragments := filepath.Join(string(cfg.State()), "ssh", "*.ssh")
	if strings.ContainsAny(fragments, " \t") {
		fragments = `"` + fragments + `"`
	}
	return "Include " + fragments
}

func PluginOf(cfg *config.Config, version string) (Plugin, error) {
	repo := slug(config.RepositoryName(cfg.Repository))
	publisher, err := publisherOf(cfg.Repository)
	if err != nil {
		return Plugin{}, err
	}
	plugin := Plugin{
		ID:          "cc-remote-recipes",
		Publisher:   publisher,
		Name:        repo + " remote workspaces",
		Version:     strings.TrimPrefix(version, "v"),
		Description: "Remote workspaces for " + repo + ", created by cc-remote over SSH.",
		Repository:  cfg.Repository,
	}
	if !semver.MatchString(plugin.Version) {
		plugin.Version = "0.0.0-" + slug(version)
	}
	return plugin, nil
}

func publisherOf(repository string) (string, error) {
	parsed, err := url.Parse(repository)
	if err != nil {
		return "", fmt.Errorf("repository %q: %w", repository, err)
	}
	if parsed.Scheme == "file" {
		return localPublisher, nil
	}
	owner, _, _ := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	publisher := slug(owner)
	if !pluginSlug.MatchString(publisher) {
		return "", fmt.Errorf("repository %q names no owner to publish the Orca plugin under", repository)
	}
	return publisher, nil
}

func slug(s string) string {
	return strings.Trim(slugBreaks.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
