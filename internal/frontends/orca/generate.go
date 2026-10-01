package orca

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"go.yaml.in/yaml/v3"
)

const (
	manifestFile = "orca-plugin.json"
	recipesKey   = "environmentRecipes"
	minimumOrca  = ">=1.4.215"
)

var ErrNotMapping = errors.New("orca.yaml is not a mapping")

type Plugin struct {
	ID          string
	Publisher   string
	Name        string
	Version     string
	Description string
	Repository  string
}

type File struct {
	Path string
	Data []byte
}

type Entry struct {
	ID           string `json:"id"                    yaml:"id"`
	Name         string `json:"name"                  yaml:"name"`
	Description  string `json:"description,omitempty"  yaml:"description,omitempty"`
	CheckoutMode string `json:"checkoutMode,omitempty" yaml:"checkoutMode,omitempty"`
	Create       string `json:"create"                yaml:"create"`
	Suspend      string `json:"suspend"               yaml:"suspend"`
	Resume       string `json:"resume"                yaml:"resume"`
	Destroy      string `json:"destroy"               yaml:"destroy"`
}

type recipeFile struct {
	SchemaVersion int `json:"schemaVersion"`
	Entry
}

type recipeRef struct {
	Path string `json:"path"`
}

type manifest struct {
	ManifestVersion int    `json:"manifestVersion"`
	ID              string `json:"id"`
	Publisher       string `json:"publisher"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	Description     string `json:"description"`
	Repository      string `json:"repository"`
	Engines         struct {
		Orca string `json:"orca"`
	} `json:"engines"`
	PluginAPI   int `json:"pluginApi"`
	Contributes struct {
		VMRecipes []recipeRef `json:"vmRecipes"`
	} `json:"contributes"`
	Capabilities []string `json:"capabilities"`
}

func DefaultPlugin() Plugin {
	return Plugin{
		ID:          "cc-remote-recipes",
		Publisher:   "yasyf",
		Name:        "cc-remote recipes",
		Version:     "0.1.0",
		Description: "Sprites and Namespace workspaces over SSH, created by cc-remote.",
		Repository:  "https://github.com/yasyf/cc-remote",
	}
}

func Entries(l Lifecycle, recipes []Recipe) ([]Entry, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	if len(recipes) == 0 {
		return nil, errors.New("no recipes configured")
	}
	seen := map[string]bool{}
	entries := make([]Entry, 0, len(recipes))
	for _, r := range recipes {
		if err := r.validate(); err != nil {
			return nil, err
		}
		if seen[r.ID()] {
			return nil, fmt.Errorf("recipe %s is configured twice", r.ID())
		}
		seen[r.ID()] = true
		entries = append(entries, Entry{
			ID:           r.ID(),
			Name:         r.Name,
			Description:  r.Description,
			CheckoutMode: provisionedRoot,
			Create:       l.Command(Create, r),
			Suspend:      l.Command(Suspend, r),
			Resume:       l.Command(Resume, r),
			Destroy:      l.Command(Destroy, r),
		})
	}
	return entries, nil
}

func PluginFiles(p Plugin, l Lifecycle, recipes []Recipe) ([]File, error) {
	entries, err := Entries(l, recipes)
	if err != nil {
		return nil, err
	}
	m := manifest{
		ManifestVersion: 1,
		ID:              p.ID,
		Publisher:       p.Publisher,
		Name:            p.Name,
		Version:         p.Version,
		Description:     p.Description,
		Repository:      p.Repository,
		PluginAPI:       1,
		Capabilities:    []string{},
	}
	m.Engines.Orca = minimumOrca
	files := make([]File, 0, len(entries)+1)
	for _, e := range entries {
		rel := path.Join("recipes", e.ID+".json")
		m.Contributes.VMRecipes = append(m.Contributes.VMRecipes, recipeRef{Path: rel})
		data, err := marshalJSON(recipeFile{SchemaVersion: 1, Entry: e})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: rel, Data: data})
	}
	data, err := marshalJSON(m)
	if err != nil {
		return nil, err
	}
	return append([]File{{Path: manifestFile, Data: data}}, files...), nil
}

func MergeYAML(existing []byte, l Lifecycle, recipes []Recipe) ([]byte, error) {
	entries, err := Entries(l, recipes)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("parse orca.yaml: %w", err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, ErrNotMapping
	}
	var value yaml.Node
	if err := value.Encode(entries); err != nil {
		return nil, fmt.Errorf("encode %s: %w", recipesKey, err)
	}
	replaced := false
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == recipesKey {
			root.Content[i+1] = &value
			replaced = true
		}
	}
	if !replaced {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: recipesKey}, &value)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode orca.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode orca.yaml: %w", err)
	}
	if _, err := ParseYAML(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("replacing %s breaks the rest of orca.yaml, likely an alias to an anchor inside it: %w", recipesKey, err)
	}
	return buf.Bytes(), nil
}

func ParseYAML(data []byte) ([]Entry, error) {
	var doc struct {
		Recipes []Entry `yaml:"environmentRecipes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse orca.yaml: %w", err)
	}
	return doc.Recipes, nil
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
