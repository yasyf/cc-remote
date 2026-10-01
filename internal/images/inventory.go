package images

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const SchemaVersion = 1

type Format string

const (
	Binary Format = "binary"
	Gzip   Format = "gzip"
	TarGz  Format = "tar.gz"
	TarXz  Format = "tar.xz"
	Zip    Format = "zip"
	Deb    Format = "deb"
)

var (
	namePattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	versionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sha512Pattern      = regexp.MustCompile(`^[0-9a-f]{128}$`)
	refPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	branchPattern      = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	githubPattern      = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	pluginPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@([A-Za-z0-9][A-Za-z0-9._-]*)$`)
	packagePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9._,-]+\])?$`)
	envPattern         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userPattern        = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	aptPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*$`)
	baseImagePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	imageNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)
	instructionPattern = regexp.MustCompile(`^([A-Z]+)[ \t]+[^\r\n]*[^\s\\]$`)
	reference          = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

type Inventory struct {
	Version      int                `yaml:"version"`
	Image        *Image             `yaml:"image"`
	Apt          Apt                `yaml:"apt"`
	System       []Artifact         `yaml:"system"`
	Tools        []Artifact         `yaml:"tools"`
	Links        []string           `yaml:"links"`
	Python       Python             `yaml:"python"`
	Claude       Claude             `yaml:"claude"`
	CodexRuntime *CodexRuntime      `yaml:"codexRuntime"`
	CaptainHook  *CaptainHook       `yaml:"captainHook"`
	Cookiesync   *Cookiesync        `yaml:"cookiesync"`
	Services     []Service          `yaml:"services"`
	Prepare      []string           `yaml:"prepare"`
	Configure    Configure          `yaml:"configure"`
	Profiles     map[string]Profile `yaml:"profiles"`
}

type Image struct {
	Name         string   `yaml:"name"`
	Base         string   `yaml:"base"`
	User         string   `yaml:"user"`
	WorkspaceDir string   `yaml:"workspaceDir"`
	Layer        []string `yaml:"layer"`
}

type Apt struct {
	Install []string `yaml:"install"`
	T64     []string `yaml:"t64"`
	Remove  []string `yaml:"remove"`
}

type Artifact struct {
	Name    string            `yaml:"name"`
	Version string            `yaml:"version"`
	URL     string            `yaml:"url"`
	SHA256  string            `yaml:"sha256"`
	SHA512  string            `yaml:"sha512"`
	Format  Format            `yaml:"format"`
	Dest    string            `yaml:"dest"`
	Bins    map[string]string `yaml:"bins"`
	Verify  []string          `yaml:"verify"`
}

type Python struct {
	Version string       `yaml:"version"`
	System  []PythonTool `yaml:"system"`
	User    []PythonTool `yaml:"user"`
}

type PythonTool struct {
	Name        string   `yaml:"name"`
	Package     string   `yaml:"package"`
	Version     string   `yaml:"version"`
	Marketplace string   `yaml:"marketplace"`
	Args        []string `yaml:"args"`
	Bins        []string `yaml:"bins"`
	Verify      []string `yaml:"verify"`
}

type Claude struct {
	ManagedSettings map[string]any    `yaml:"managedSettings"`
	Env             map[string]string `yaml:"env"`
	Marketplaces    []Marketplace     `yaml:"marketplaces"`
	Plugins         []Plugin          `yaml:"plugins"`
}

type Marketplace struct {
	Name    string `yaml:"name"`
	GitHub  string `yaml:"github"`
	Ref     string `yaml:"ref"`
	Branch  string `yaml:"branch"`
	Private bool   `yaml:"private"`
}

type Plugin struct {
	ID      string   `yaml:"id"`
	Version string   `yaml:"version"`
	Bins    []string `yaml:"bins"`
}

type CodexRuntime struct {
	Version string   `yaml:"version"`
	URL     string   `yaml:"url"`
	SHA256  string   `yaml:"sha256"`
	Plugins []string `yaml:"plugins"`
}

type CaptainHook struct {
	Version string `yaml:"version"`
	URL     string `yaml:"url"`
	SHA256  string `yaml:"sha256"`
}

type Cookiesync struct {
	SchemaFingerprint string `yaml:"schemaFingerprint"`
}

type Service struct {
	Name    string            `yaml:"name"`
	Plugin  string            `yaml:"plugin"`
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

type Configure struct {
	Env []string `yaml:"env"`
	Run []string `yaml:"run"`
}

type Profile struct {
	Tools   []Artifact `yaml:"tools"`
	Prepare []string   `yaml:"prepare"`
}

func Load(file string) (Inventory, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Inventory{}, fmt.Errorf("read inventory: %w", err)
	}
	return Parse(raw)
}

func Parse(raw []byte) (Inventory, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var inventory Inventory
	if err := decoder.Decode(&inventory); err != nil {
		return Inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	switch err := decoder.Decode(new(any)); {
	case err == nil:
		return Inventory{}, errors.New("parse inventory: want one YAML document")
	case !errors.Is(err, io.EOF):
		return Inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	if err := inventory.Validate(); err != nil {
		return Inventory{}, fmt.Errorf("inventory: %w", err)
	}
	return inventory, nil
}

func (inv Inventory) Validate() error {
	if inv.Version != SchemaVersion {
		return fmt.Errorf("version is %d, want %d", inv.Version, SchemaVersion)
	}
	checks := []func() error{
		inv.validateImage,
		inv.validateApt,
		inv.validateArtifacts,
		inv.validatePython,
		inv.validateClaude,
		inv.validateExtras,
		inv.validateServices,
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (inv Inventory) validateImage() error {
	if inv.Image == nil {
		return nil
	}
	image := *inv.Image
	switch {
	case !imageNamePattern.MatchString(image.Name):
		return fmt.Errorf("image.name %q is not a lowercase image name", image.Name)
	case !baseImagePattern.MatchString(image.Base):
		return fmt.Errorf("image.base %q is not pinned by @sha256 digest", image.Base)
	case !userPattern.MatchString(image.User):
		return fmt.Errorf("image.user %q is not a Linux user name", image.User)
	case !path.IsAbs(image.WorkspaceDir) || path.Clean(image.WorkspaceDir) != image.WorkspaceDir:
		return fmt.Errorf("image.workspaceDir %q is not a clean absolute path", image.WorkspaceDir)
	}
	for _, line := range image.Layer {
		match := instructionPattern.FindStringSubmatch(line)
		switch {
		case match == nil:
			return fmt.Errorf("image.layer: %q is not one Dockerfile instruction on one line", line)
		case match[1] == "FROM":
			return errors.New("image.layer: FROM would start a new stage after the provisioned one")
		case strings.Contains(line, "<<"):
			return fmt.Errorf("image.layer: %q opens a heredoc, which would swallow the lines after it", line)
		}
	}
	return nil
}

func (inv Inventory) validateApt() error {
	lists := []struct {
		field string
		names []string
	}{{"install", inv.Apt.Install}, {"t64", inv.Apt.T64}, {"remove", inv.Apt.Remove}}
	for _, list := range lists {
		for _, name := range list.names {
			if !aptPattern.MatchString(name) {
				return fmt.Errorf("apt.%s: %q is not a Debian package name", list.field, name)
			}
		}
	}
	return nil
}

func (inv Inventory) validateArtifacts() error {
	systemBins := map[string]string{}
	systemDirs := destinations{}
	for _, artifact := range inv.System {
		if err := artifact.validate("system", true); err != nil {
			return err
		}
		owner := "system[" + artifact.Name + "]"
		if err := claim(systemBins, artifact.links(), owner); err != nil {
			return err
		}
		if err := systemDirs.take(artifact.destination(true), owner); err != nil {
			return err
		}
	}
	for _, tool := range inv.Python.System {
		if err := claim(systemBins, tool.Bins, "python.system["+tool.Name+"]"); err != nil {
			return err
		}
	}
	userBins := map[string]string{}
	for _, link := range inv.Links {
		if _, ok := systemBins[link]; !ok {
			return fmt.Errorf("links: %q is not a system bin", link)
		}
		if err := claim(userBins, []string{link}, "links"); err != nil {
			return err
		}
	}
	for _, tool := range inv.Python.User {
		if err := claim(userBins, tool.Bins, "python.user["+tool.Name+"]"); err != nil {
			return err
		}
	}
	userDirs := destinations{}
	for _, reserved := range []string{".cc-remote", ".local/bin", ".local/share/cc-remote/marketplaces"} {
		if err := userDirs.take(destination{"$HOME", reserved}, "cc-remote"); err != nil {
			return err
		}
	}
	for _, artifact := range inv.Tools {
		if err := artifact.validate("tools", false); err != nil {
			return err
		}
		owner := "tools[" + artifact.Name + "]"
		if err := claim(userBins, artifact.links(), owner); err != nil {
			return err
		}
		if err := userDirs.take(artifact.destination(false), owner); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(inv.Profiles)) {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("profiles: %q is not a profile name", name)
		}
		profileBins := maps.Clone(userBins)
		profileDirs := userDirs.clone()
		for _, artifact := range inv.Profiles[name].Tools {
			where := "profiles." + name + ".tools"
			if err := artifact.validate(where, false); err != nil {
				return err
			}
			owner := where + "[" + artifact.Name + "]"
			if err := claim(profileBins, artifact.links(), owner); err != nil {
				return err
			}
			if err := profileDirs.take(artifact.destination(false), owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func claim(owners map[string]string, bins []string, owner string) error {
	for _, bin := range bins {
		if !namePattern.MatchString(bin) {
			return fmt.Errorf("%s: bin %q is not a file name", owner, bin)
		}
		if previous, ok := owners[bin]; ok {
			return fmt.Errorf("%s: bin %q is already installed by %s", owner, bin, previous)
		}
		owners[bin] = owner
	}
	return nil
}

type destination struct {
	root string
	dir  string
}

type destinations map[string][]claimedDir

type claimedDir struct {
	dir   string
	owner string
}

func (d destinations) take(dest destination, owner string) error {
	for _, prior := range d[dest.root] {
		if prior.dir == dest.dir || strings.HasPrefix(dest.dir, prior.dir+"/") || strings.HasPrefix(prior.dir, dest.dir+"/") {
			return fmt.Errorf("%s: installs into %s/%s, which overlaps %s/%s owned by %s", owner, dest.root, dest.dir, dest.root, prior.dir, prior.owner)
		}
	}
	d[dest.root] = append(d[dest.root], claimedDir{dest.dir, owner})
	return nil
}

func (d destinations) clone() destinations {
	clone := destinations{}
	for root, dirs := range d {
		clone[root] = slices.Clone(dirs)
	}
	return clone
}

func (a Artifact) destination(system bool) destination {
	switch {
	case system:
		return destination{"/opt/cc-remote/tools", a.dir()}
	case a.Dest != "":
		return destination{"$HOME", a.Dest}
	}
	return destination{"$HOME", ".local/share/cc-remote/tools/" + a.dir()}
}

func (a Artifact) validate(where string, system bool) error {
	where = where + "[" + a.Name + "]"
	switch {
	case !namePattern.MatchString(a.Name):
		return fmt.Errorf("%s: name %q is not a file name", where, a.Name)
	case !versionPattern.MatchString(a.Version):
		return fmt.Errorf("%s: version %q is not a version", where, a.Version)
	case (a.SHA256 == "") == (a.SHA512 == ""):
		return fmt.Errorf("%s: set exactly one of sha256 and sha512", where)
	case a.SHA256 != "" && !sha256Pattern.MatchString(a.SHA256):
		return fmt.Errorf("%s: sha256 is not 64 lowercase hex digits", where)
	case a.SHA512 != "" && !sha512Pattern.MatchString(a.SHA512):
		return fmt.Errorf("%s: sha512 is not 128 lowercase hex digits", where)
	case a.Format == Deb && !system:
		return fmt.Errorf("%s: a deb installs system-wide, so it belongs under system", where)
	case a.Dest != "" && system:
		return fmt.Errorf("%s: dest applies only to tools", where)
	case a.Dest != "" && !relative(a.Dest):
		return fmt.Errorf("%s: dest %q is not a clean path relative to $HOME", where, a.Dest)
	}
	if err := httpsURL(a.URL); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	switch a.Format {
	case Binary, Gzip:
		for bin, member := range a.Bins {
			if member != a.Name {
				return fmt.Errorf("%s: bin %q must point at %q, the single file a %s artifact installs", where, bin, a.Name, a.Format)
			}
		}
	case TarGz, TarXz, Zip:
		if len(a.Bins) == 0 {
			return fmt.Errorf("%s: an archive needs bins naming the members to link", where)
		}
		for bin, member := range a.Bins {
			if !relative(member) {
				return fmt.Errorf("%s: bin %q member %q is not a clean relative path", where, bin, member)
			}
		}
	case Deb:
		if len(a.Bins) == 0 {
			return fmt.Errorf("%s: a deb needs bins naming the absolute paths to link", where)
		}
		for bin, target := range a.Bins {
			if !path.IsAbs(target) || path.Clean(target) != target {
				return fmt.Errorf("%s: bin %q target %q is not a clean absolute path", where, bin, target)
			}
		}
	default:
		return fmt.Errorf("%s: format %q is not one of binary, gzip, tar.gz, tar.xz, zip, deb", where, a.Format)
	}
	return nil
}

func (a Artifact) links() []string {
	if len(a.Bins) == 0 {
		return []string{a.Name}
	}
	return slices.Sorted(maps.Keys(a.Bins))
}

func (a Artifact) bins() map[string]string {
	if len(a.Bins) == 0 {
		return map[string]string{a.Name: a.Name}
	}
	return a.Bins
}

func (a Artifact) digest() (string, string) {
	if a.SHA256 != "" {
		return "sha256", a.SHA256
	}
	return "sha512", a.SHA512
}

func (a Artifact) dir() string {
	if a.Dest != "" {
		return a.Dest
	}
	return a.Name + "-" + a.Version
}

func (inv Inventory) validatePython() error {
	tools := slices.Concat(inv.Python.System, inv.Python.User)
	if len(tools) > 0 && !versionPattern.MatchString(inv.Python.Version) {
		return fmt.Errorf("python.version %q is not a version", inv.Python.Version)
	}
	if len(inv.Python.System) > 0 && !inv.providesSystem("uv") {
		return errors.New("python.system: no system artifact provides the uv bin")
	}
	if len(tools) > 0 && !inv.provides("uv") {
		return errors.New("python: no system or tools artifact provides the uv bin")
	}
	seen := map[string]bool{}
	for i, tool := range tools {
		system := i < len(inv.Python.System)
		where := "python.user[" + tool.Name + "]"
		if system {
			where = "python.system[" + tool.Name + "]"
		}
		switch {
		case !namePattern.MatchString(tool.Name):
			return fmt.Errorf("%s: name %q is not a tool name", where, tool.Name)
		case seen[tool.Name]:
			return fmt.Errorf("%s: duplicate tool", where)
		case tool.Package != "" && !packagePattern.MatchString(tool.Package):
			return fmt.Errorf("%s: package %q is not a Python requirement name", where, tool.Package)
		case (tool.Version == "") == (tool.Marketplace == ""):
			return fmt.Errorf("%s: set exactly one of version and marketplace", where)
		case tool.Version != "" && !versionPattern.MatchString(tool.Version):
			return fmt.Errorf("%s: version %q is not a version", where, tool.Version)
		case tool.Marketplace != "" && system:
			return fmt.Errorf("%s: marketplace checkouts live in the user's home, so only python.user may install from one", where)
		case tool.Marketplace != "" && !slices.ContainsFunc(inv.Claude.Marketplaces, func(m Marketplace) bool { return m.Name == tool.Marketplace }):
			return fmt.Errorf("%s: marketplace %q is not under claude.marketplaces", where, tool.Marketplace)
		case tool.Marketplace != "" && slices.ContainsFunc(inv.Claude.Marketplaces, func(m Marketplace) bool { return m.Name == tool.Marketplace && m.Branch != "" && m.Ref == "" }):
			return fmt.Errorf("%s: marketplace %q is registered by branch, so it has no pinned checkout to install from", where, tool.Marketplace)
		case len(tool.Bins) == 0:
			return fmt.Errorf("%s: bins names no executable to verify", where)
		}
		seen[tool.Name] = true
	}
	return nil
}

func (inv Inventory) validateClaude() error {
	if (len(inv.Claude.Marketplaces) > 0 || len(inv.Claude.Plugins) > 0) && !inv.provides("claude") {
		return errors.New("claude: no system or tools artifact provides the claude bin")
	}
	marketplaces := map[string]bool{}
	for _, marketplace := range inv.Claude.Marketplaces {
		where := "claude.marketplaces[" + marketplace.Name + "]"
		switch {
		case !namePattern.MatchString(marketplace.Name):
			return fmt.Errorf("%s: name is not a marketplace name", where)
		case marketplaces[marketplace.Name]:
			return fmt.Errorf("%s: duplicate marketplace", where)
		case !githubPattern.MatchString(marketplace.GitHub):
			return fmt.Errorf("%s: github %q is not owner/repo", where, marketplace.GitHub)
		case (marketplace.Ref == "") == (marketplace.Branch == ""):
			return fmt.Errorf("%s: set exactly one of ref and branch", where)
		case marketplace.Ref != "" && !refPattern.MatchString(marketplace.Ref):
			return fmt.Errorf("%s: ref %q is not a 40-digit commit", where, marketplace.Ref)
		case marketplace.Branch != "" && !branchName(marketplace.Branch):
			return fmt.Errorf("%s: branch %q is not a branch name", where, marketplace.Branch)
		case marketplace.Branch != "" && marketplace.Private:
			return fmt.Errorf("%s: Claude Code clones a branch marketplace itself, so a private one needs a token-fetched ref", where)
		}
		marketplaces[marketplace.Name] = true
	}
	plugins := map[string]bool{}
	for _, plugin := range inv.Claude.Plugins {
		where := "claude.plugins[" + plugin.ID + "]"
		match := pluginPattern.FindStringSubmatch(plugin.ID)
		switch {
		case match == nil:
			return fmt.Errorf("%s: id is not name@marketplace", where)
		case !marketplaces[match[1]]:
			return fmt.Errorf("%s: marketplace %q is not under claude.marketplaces", where, match[1])
		case plugins[plugin.ID]:
			return fmt.Errorf("%s: duplicate plugin", where)
		case !versionPattern.MatchString(plugin.Version):
			return fmt.Errorf("%s: version %q is not a version", where, plugin.Version)
		}
		for _, bin := range plugin.Bins {
			if !relative(bin) {
				return fmt.Errorf("%s: bin %q is not a clean path relative to the plugin root", where, bin)
			}
		}
		plugins[plugin.ID] = true
	}
	for _, key := range slices.Sorted(maps.Keys(inv.Claude.Env)) {
		if err := inv.validateEnv("claude.env", key, inv.Claude.Env[key]); err != nil {
			return err
		}
	}
	return nil
}

func (inv Inventory) validateExtras() error {
	if runtime := inv.CodexRuntime; runtime != nil {
		switch {
		case !inv.provides("codex"):
			return errors.New("codexRuntime: no system or tools artifact provides the codex bin")
		case !versionPattern.MatchString(runtime.Version):
			return fmt.Errorf("codexRuntime: version %q is not a version", runtime.Version)
		case !sha256Pattern.MatchString(runtime.SHA256):
			return errors.New("codexRuntime: sha256 is not 64 lowercase hex digits")
		}
		if err := httpsURL(runtime.URL); err != nil {
			return fmt.Errorf("codexRuntime: %w", err)
		}
		for _, plugin := range runtime.Plugins {
			if !namePattern.MatchString(plugin) {
				return fmt.Errorf("codexRuntime: plugin %q is not a plugin name", plugin)
			}
		}
	}
	if hook := inv.CaptainHook; hook != nil {
		switch {
		case !inv.provides("uv"):
			return errors.New("captainHook: no system or tools artifact provides the uv bin its package install runs")
		case !versionPattern.MatchString(hook.Version):
			return fmt.Errorf("captainHook: version %q is not a version", hook.Version)
		case !sha256Pattern.MatchString(hook.SHA256):
			return errors.New("captainHook: sha256 is not 64 lowercase hex digits")
		}
		if err := httpsURL(hook.URL); err != nil {
			return fmt.Errorf("captainHook: %w", err)
		}
	}
	if sync := inv.Cookiesync; sync != nil {
		switch {
		case !inv.provides("cookiesync"):
			return errors.New("cookiesync: no system or tools artifact provides the cookiesync bin")
		case !sha256Pattern.MatchString(sync.SchemaFingerprint):
			return errors.New("cookiesync: schemaFingerprint is not 64 lowercase hex digits")
		}
	}
	return nil
}

func (inv Inventory) validateServices() error {
	declared := map[string]bool{}
	for _, name := range inv.Configure.Env {
		switch {
		case !envPattern.MatchString(name):
			return fmt.Errorf("configure.env: %q is not an environment variable name", name)
		case declared[name]:
			return fmt.Errorf("configure.env: duplicate %q", name)
		}
		declared[name] = true
	}
	if err := validateSteps("configure.run", inv.Configure.Run); err != nil {
		return err
	}
	if err := validateSteps("prepare", inv.Prepare); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(inv.Profiles)) {
		if err := validateSteps("profiles."+name+".prepare", inv.Profiles[name].Prepare); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, service := range inv.Services {
		where := "services[" + service.Name + "]"
		switch {
		case !namePattern.MatchString(service.Name):
			return fmt.Errorf("%s: name is not a service name", where)
		case names[service.Name]:
			return fmt.Errorf("%s: duplicate service", where)
		case len(service.Command) == 0:
			return fmt.Errorf("%s: command is empty", where)
		case service.Plugin == "" && !namePattern.MatchString(service.Command[0]):
			return fmt.Errorf("%s: command %q is not a bin name on PATH", where, service.Command[0])
		case service.Plugin != "" && !relative(service.Command[0]):
			return fmt.Errorf("%s: command %q is not a clean path relative to the plugin root", where, service.Command[0])
		case service.Plugin != "" && !slices.ContainsFunc(inv.Claude.Plugins, func(p Plugin) bool { return p.ID == service.Plugin }):
			return fmt.Errorf("%s: plugin %q is not under claude.plugins", where, service.Plugin)
		}
		for _, key := range slices.Sorted(maps.Keys(service.Env)) {
			if err := inv.validateEnv(where+".env", key, service.Env[key]); err != nil {
				return err
			}
		}
		names[service.Name] = true
	}
	return nil
}

func validateSteps(where string, steps []string) error {
	for _, step := range steps {
		if strings.TrimSpace(step) == "" {
			return fmt.Errorf("%s: a step is empty", where)
		}
	}
	return nil
}

func (inv Inventory) validateEnv(where, key, value string) error {
	if !envPattern.MatchString(key) {
		return fmt.Errorf("%s: %q is not an environment variable name", where, key)
	}
	for _, name := range references(value) {
		if !slices.Contains(inv.Configure.Env, name) {
			return fmt.Errorf("%s: %s references ${%s}, which configure.env does not declare", where, key, name)
		}
	}
	return nil
}

func (inv Inventory) provides(bin string) bool {
	for _, artifact := range slices.Concat(inv.System, inv.Tools) {
		if _, ok := artifact.bins()[bin]; ok {
			return true
		}
	}
	return false
}

func (inv Inventory) providesSystem(bin string) bool {
	return slices.ContainsFunc(inv.System, func(a Artifact) bool {
		_, ok := a.bins()[bin]
		return ok
	})
}

func branchName(branch string) bool {
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".") {
		return false
	}
	for component := range strings.SplitSeq(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func references(template string) []string {
	matches := reference.FindAllStringSubmatch(template, -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	return names
}

func relative(p string) bool {
	return p != "" && p != "." && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

func httpsURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(raw, " \t\n") {
		return fmt.Errorf("url %q is not an https URL", raw)
	}
	return nil
}
