package images

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	assets "github.com/yasyf/cc-remote/images"
)

const (
	StartPath   = "/usr/local/bin/cc-remote-start"
	PluginsPath = "$HOME/.cc-remote/plugins.sh"

	toolDomain  = "cc-remote/tools/v1"
	imageDomain = "cc-remote/image/v1"
	stampDomain = "cc-remote/ready/v1"
)

type Scripts struct {
	ProvisionScript []byte
	Plugins         []byte
	Env             []string
}

type Context struct {
	Image      Image
	Dockerfile []byte
	Provision  []byte
	Start      []byte
}

type exposure string

const (
	exposeLink     exposure = "link"
	exposeChildren exposure = "children"
	exposeCopy     exposure = "copy"
)

type requirement string

const (
	required requirement = "required"
	optional requirement = "optional"
)

type tree struct {
	Kind        exposure
	Requirement requirement
	Path        string
}

var readyTimeout = 30 * time.Second

type native struct {
	ID  string
	Ref string
	Dir string
	Bin string
}

type view struct {
	Inventory
	Tools        []Artifact
	Prepare      []string
	SystemTrees  []tree
	HomeTrees    []tree
	ReadyTimeout float64
	Natives      []native
}

func newView(inv Inventory, profile string) view {
	tools := slices.Concat(inv.Tools, inv.Profiles[profile].Tools)
	return view{
		Inventory:    inv,
		Tools:        tools,
		Prepare:      slices.Concat(inv.Prepare, inv.Profiles[profile].Prepare),
		SystemTrees:  systemTrees(inv),
		HomeTrees:    homeTrees(inv, tools),
		ReadyTimeout: readyTimeout.Seconds(),
		Natives:      natives(inv),
	}
}

func newSystemView(inv Inventory) view {
	return view{Inventory: inv, SystemTrees: systemTrees(inv)}
}

func systemTrees(inv Inventory) []tree {
	var trees []tree
	for _, a := range inv.System {
		if a.Format != Deb {
			trees = append(trees, tree{exposeLink, required, "/opt/cc-remote/tools/" + a.dir()})
		}
	}
	for _, tool := range inv.Python.System {
		trees = append(trees, tree{exposeLink, required, "/opt/uv/tools/" + uvToolDir(tool)})
		for _, bin := range tool.Bins {
			trees = append(trees, tree{exposeCopy, required, "/usr/local/bin/" + bin})
		}
	}
	if len(inv.Python.System) > 0 {
		trees = append(trees, tree{exposeLink, required, "/opt/uv/python"})
	}
	return trees
}

func homeTrees(inv Inventory, tools []Artifact) []tree {
	var trees []tree
	for _, a := range tools {
		rel := ".local/share/cc-remote/tools/" + a.dir()
		if a.Dest != "" {
			rel = a.Dest
		}
		trees = append(trees, tree{exposeLink, required, rel})
	}
	settings := len(inv.Claude.Plugins) > 0
	for _, marketplace := range inv.Claude.Marketplaces {
		if marketplace.Ref != "" {
			trees = append(trees, tree{exposeLink, required, ".local/share/cc-remote/marketplaces/" + marketplace.Name})
		} else {
			trees = append(trees, tree{exposeCopy, required, ".claude/plugins/marketplaces/" + marketplace.Name})
			settings = true
		}
	}
	for _, plugin := range inv.Claude.Plugins {
		trees = append(trees, tree{exposeCopy, required, pluginCacheDir(plugin)})
	}
	if len(inv.Claude.Plugins) > 0 {
		trees = append(trees, tree{exposeCopy, required, ".claude/plugins/installed_plugins.json"})
	}
	if len(inv.Claude.Marketplaces) > 0 {
		trees = append(trees, tree{exposeCopy, required, ".claude/plugins/known_marketplaces.json"})
	}
	if settings {
		trees = append(trees, tree{exposeCopy, required, ".claude/settings.json"})
	}
	if inv.CodexRuntime != nil {
		trees = append(trees,
			tree{exposeLink, required, ".cache/codex-runtimes/codex-primary-runtime"},
			tree{exposeCopy, required, ".codex/config.toml"},
			tree{exposeCopy, required, ".codex/plugins/cache/openai-primary-runtime"},
		)
	}
	if inv.CaptainHook != nil {
		trees = append(trees, tree{exposeChildren, required, ".daemonkit/tools/capt-hook/" + inv.CaptainHook.Version})
	}
	if inv.CaptainHook != nil || len(inv.Python.User) > 0 {
		trees = append(trees, tree{exposeLink, optional, ".local/share/uv/python"})
	}
	return trees
}

func pluginCacheDir(plugin Plugin) string {
	name, marketplace, _ := strings.Cut(plugin.ID, "@")
	return ".claude/plugins/cache/" + marketplace + "/" + name + "/" + plugin.Version
}

func natives(inv Inventory) []native {
	refs := map[string]string{}
	for _, marketplace := range inv.Claude.Marketplaces {
		refs[marketplace.Name] = marketplace.Ref
	}
	var candidates []native
	for _, plugin := range inv.Claude.Plugins {
		name, marketplace, _ := strings.Cut(plugin.ID, "@")
		if name == "captain-hook" || refs[marketplace] == "" {
			continue
		}
		bins := slices.Clone(plugin.Bins)
		for _, service := range inv.Services {
			if service.Plugin == plugin.ID {
				bins = append(bins, service.Command[0])
			}
		}
		for _, bin := range bins {
			candidate := native{plugin.ID, refs[marketplace], pluginCacheDir(plugin), bin}
			if !slices.Contains(candidates, candidate) {
				candidates = append(candidates, candidate)
			}
		}
	}
	return candidates
}

func uvToolDir(tool PythonTool) string {
	name, _, _ := strings.Cut(cmp.Or(tool.Package, tool.Name), "[")
	return name
}

func Render(inv Inventory, profile string) (Scripts, error) {
	bound, err := parse(inv)
	if err != nil {
		return Scripts{}, err
	}
	data := newView(inv, profile)
	provision, err := execute(bound, "provision.sh", data)
	if err != nil {
		return Scripts{}, err
	}
	plugins, err := execute(bound, "plugins.sh", data)
	if err != nil {
		return Scripts{}, err
	}
	return Scripts{ProvisionScript: provision, Plugins: plugins, Env: slices.Clone(inv.Configure.Env)}, nil
}

func RenderImage(inv Inventory) (Context, error) {
	if inv.Image == nil {
		return Context{}, errors.New("inventory has no image section")
	}
	bound, err := parse(inv)
	if err != nil {
		return Context{}, err
	}
	dockerfile, err := execute(bound, "Dockerfile", *inv.Image)
	if err != nil {
		return Context{}, err
	}
	provision, err := execute(bound, "provision.sh", newSystemView(inv))
	if err != nil {
		return Context{}, err
	}
	start, err := assets.FS.ReadFile("start.sh")
	if err != nil {
		return Context{}, err
	}
	return Context{Image: *inv.Image, Dockerfile: dockerfile, Provision: provision, Start: start}, nil
}

func (s Scripts) Fingerprint() string {
	return fingerprint(toolDomain, file{"provision.sh", s.ProvisionScript}, file{"plugins.sh", s.Plugins})
}

func Stamp(scripts Scripts, image *Context, payload string) string {
	files := []file{{"tools", []byte(scripts.Fingerprint())}}
	if image != nil {
		files = append(files, file{"image", []byte(image.Fingerprint())})
	}
	if payload != "" {
		files = append(files, file{"payload", []byte(payload)})
	}
	return fingerprint(stampDomain, files...)
}

func (c Context) Fingerprint() string {
	return fingerprint(imageDomain, c.files()...)
}

func (c Context) Write(dir string) error {
	for _, f := range c.files() {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return fmt.Errorf("write image context: %w", err)
		}
	}
	return nil
}

func (c Context) files() []file {
	return []file{{"Dockerfile", c.Dockerfile}, {"provision.sh", c.Provision}, {"start.sh", c.Start}}
}

type file struct {
	name string
	data []byte
}

func fingerprint(domain string, files ...file) string {
	hash := sha256.New()
	hash.Write(fmt.Appendf(nil, "%s\n", domain))
	for _, f := range files {
		hash.Write(fmt.Appendf(nil, "%s %d\n", f.name, len(f.data)))
		hash.Write(f.data)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func parse(inv Inventory) (*template.Template, error) {
	refs := map[string]string{}
	for _, marketplace := range inv.Claude.Marketplaces {
		refs[marketplace.Name] = marketplace.Ref
	}
	spec := func(tool PythonTool) string {
		pkg := cmp.Or(tool.Package, tool.Name)
		if tool.Version != "" {
			return quote(pkg + "==" + tool.Version)
		}
		return quote(pkg+" @ git+file://") + `"$marketplace_dir"` + quote("/"+tool.Marketplace+"@"+refs[tool.Marketplace])
	}
	t, err := template.New("").Funcs(template.FuncMap{
		"q":          quote,
		"json":       indentJSON,
		"install":    installCall,
		"verify":     verifyCalls,
		"expand":     expand,
		"executable": serviceExecutable,
		"spec":       spec,
		"under":      under,
		"home": func(rel string) string {
			return under("HOME", rel)
		},
		"pins": func(marketplace string) []string {
			var pins []string
			for _, plugin := range inv.Claude.Plugins {
				if _, owner, _ := strings.Cut(plugin.ID, "@"); owner == marketplace {
					pins = append(pins, plugin.ID+" "+plugin.Version)
				}
			}
			return pins
		},
		"pluginRef": func(plugin Plugin) string {
			_, marketplace, _ := strings.Cut(plugin.ID, "@")
			return refs[marketplace]
		},
	}).ParseFS(assets.FS, "artifacts.sh", "provision.sh", "plugins.sh", "supervise.py", "namespace/Dockerfile")
	if err != nil {
		return nil, fmt.Errorf("parse image templates: %w", err)
	}
	return t, nil
}

func execute(t *template.Template, name string, data any) ([]byte, error) {
	var out bytes.Buffer
	if err := t.ExecuteTemplate(&out, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return out.Bytes(), nil
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func expand(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	last := 0
	for _, match := range reference.FindAllStringSubmatchIndex(s, -1) {
		out.WriteString(escapeDoubleQuoted(s[last:match[0]]))
		out.WriteString("${" + s[match[2]:match[3]] + "}")
		last = match[1]
	}
	out.WriteString(escapeDoubleQuoted(s[last:]))
	out.WriteByte('"')
	return out.String()
}

func escapeDoubleQuoted(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

func indentJSON(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render managed settings: %w", err)
	}
	return string(out), nil
}

func under(dirVar, rel string) string {
	return `"$` + dirVar + `/"` + quote(rel)
}

func artifactDir(a Artifact, toolDir string) string {
	if a.Dest != "" {
		return under("HOME", a.Dest)
	}
	return under(toolDir, a.dir())
}

func installCall(a Artifact, toolDir, binDir string) string {
	algorithm, digest := a.digest()
	bins := a.bins()
	words := make([]string, 0, 9+2*len(bins))
	words = append(words,
		"install_artifact", quote(a.Name), quote(a.Version), quote(a.URL), algorithm, quote(digest), string(a.Format),
		artifactDir(a, toolDir), `"$`+binDir+`"`,
	)
	for _, bin := range slices.Sorted(maps.Keys(bins)) {
		words = append(words, quote(bin), quote(bins[bin]))
	}
	return strings.Join(words, " ")
}

func verifyCalls(a Artifact, toolDir, binDir string) []string {
	_, digest := a.digest()
	bins := a.bins()
	calls := make([]string, 0, 1+len(bins))
	calls = append(calls, "verify_pin "+artifactDir(a, toolDir)+" "+quote(digest))
	for _, bin := range slices.Sorted(maps.Keys(bins)) {
		target := quote(bins[bin])
		if a.Format != Deb {
			if a.Dest != "" {
				target = under("HOME", a.Dest+"/"+bins[bin])
			} else {
				target = under(toolDir, a.dir()+"/"+bins[bin])
			}
		}
		words := make([]string, 0, 3+len(a.Verify))
		words = append(words, "verify_link", under(binDir, bin), target)
		for _, arg := range a.Verify {
			words = append(words, quote(arg))
		}
		calls = append(calls, strings.Join(words, " "))
	}
	return calls
}

func serviceExecutable(s Service) string {
	if s.Plugin != "" {
		return `"$(plugin_path "$plugins" ` + quote(s.Plugin) + `)/"` + quote(s.Command[0])
	}
	return `"$(command -v ` + quote(s.Command[0]) + `)"`
}
