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

	assets "github.com/yasyf/cc-remote/images"
)

const (
	StartPath   = "/usr/local/bin/cc-remote-start"
	PluginsPath = "$HOME/.cc-remote/plugins.sh"

	toolDomain  = "cc-remote/tools/v1"
	imageDomain = "cc-remote/image/v1"
)

type Scripts struct {
	Provision []byte
	Plugins   []byte
	Env       []string
}

type Context struct {
	Image      Image
	Dockerfile []byte
	Provision  []byte
	Start      []byte
}

type view struct {
	Inventory
	Tools []Artifact
}

func Render(inv Inventory, profile string) (Scripts, error) {
	bound, err := parse(inv)
	if err != nil {
		return Scripts{}, err
	}
	data := view{Inventory: inv, Tools: slices.Concat(inv.Tools, inv.Profiles[profile].Tools)}
	provision, err := execute(bound, "provision.sh", data)
	if err != nil {
		return Scripts{}, err
	}
	plugins, err := execute(bound, "plugins.sh", data)
	if err != nil {
		return Scripts{}, err
	}
	return Scripts{Provision: provision, Plugins: plugins, Env: slices.Clone(inv.Configure.Env)}, nil
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
	provision, err := execute(bound, "provision.sh", view{Inventory: inv})
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
	return fingerprint(toolDomain, file{"provision.sh", s.Provision}, file{"plugins.sh", s.Plugins})
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
		return `"$(plugin_root ` + quote(s.Plugin) + `)/"` + quote(s.Command[0])
	}
	return `"$(command -v ` + quote(s.Command[0]) + `)"`
}
