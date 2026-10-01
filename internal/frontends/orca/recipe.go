package orca

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/yasyf/cc-remote/internal/providers"
)

type Verb string

const (
	Create  Verb = "create"
	Suspend Verb = "suspend"
	Resume  Verb = "resume"
	Destroy Verb = "destroy"
)

const (
	connection      = "ssh"
	provisionedRoot = "provisioned-root"
)

var (
	recipeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	segment  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

type Source struct {
	Provider string
	Profile  string
	Profiles map[string][]string
}

type Recipe struct {
	Provider    string
	Profile     string
	Name        string
	Description string
}

type Lifecycle struct {
	Binary string
	Config string
}

type machine struct {
	provider string
	profile  string
}

func DefaultSource() Source {
	return Source{
		Provider: "sprites",
		Profile:  "lean",
		Profiles: map[string][]string{
			"lean": {"sprites", "namespace"},
			"full": {"namespace"},
		},
	}
}

func DefaultLifecycle() Lifecycle {
	return Lifecycle{Binary: "cc-remote"}
}

func Recipes(src Source) ([]Recipe, error) {
	if !slices.Contains(src.Profiles[src.Profile], src.Provider) {
		return nil, fmt.Errorf("default profile %q configures no %s machine", src.Profile, src.Provider)
	}
	fallback := machine{provider: src.Provider, profile: src.Profile}
	var machines []machine
	for profile, providers := range src.Profiles {
		for _, provider := range providers {
			if !segment.MatchString(provider) || !segment.MatchString(profile) {
				return nil, fmt.Errorf("provider %q and profile %q must match %s", provider, profile, segment)
			}
			machines = append(machines, machine{provider: provider, profile: profile})
		}
	}
	slices.SortFunc(machines, func(a, b machine) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.profile, b.profile))
	})
	i := slices.Index(machines, fallback)
	machines = append([]machine{fallback}, slices.Delete(machines, i, i+1)...)
	recipes := make([]Recipe, 0, len(machines))
	for _, m := range machines {
		recipes = append(recipes, m.recipe(m == fallback))
	}
	return recipes, nil
}

func (m machine) recipe(isDefault bool) Recipe {
	provider := strings.ToUpper(m.provider[:1]) + m.provider[1:]
	name := fmt.Sprintf("%s %s over SSH", provider, m.profile)
	description := fmt.Sprintf("A %s machine with the %s profile, reached over SSH.", provider, m.profile)
	if isDefault {
		name += " (default)"
		description = "The default. " + description
	}
	return Recipe{Provider: m.provider, Profile: m.profile, Name: name, Description: description}
}

func (r Recipe) ID() string {
	return r.Provider + "-" + r.Profile + "-" + connection
}

func (r Recipe) validate() error {
	for field, value := range map[string]string{"provider": r.Provider, "profile": r.Profile} {
		if !segment.MatchString(value) {
			return fmt.Errorf("recipe %s: %s %q must match %s", r.ID(), field, value, segment)
		}
	}
	if !recipeID.MatchString(r.ID()) {
		return fmt.Errorf("recipe id %q must match Orca's %s", r.ID(), recipeID)
	}
	if r.Name == "" {
		return fmt.Errorf("recipe %s: name is empty", r.ID())
	}
	return nil
}

func (l Lifecycle) validate() error {
	if l.Binary == "" {
		return errors.New("lifecycle binary is empty")
	}
	for _, value := range []string{l.Binary, l.Config} {
		if strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("lifecycle path %q contains a control character", value)
		}
	}
	return nil
}

func (l Lifecycle) Command(verb Verb, r Recipe) string {
	words := []string{l.Binary, string(verb), "--provider", r.Provider, "--profile", r.Profile, "--connection", connection}
	if l.Config != "" {
		words = append(words, "--config", l.Config)
	}
	return providers.ShellQuote(words...)
}

func FindRecipe(recipes []Recipe, id string) (Recipe, error) {
	ids := make([]string, 0, len(recipes))
	for _, r := range recipes {
		if r.ID() == id {
			return r, nil
		}
		ids = append(ids, r.ID())
	}
	return Recipe{}, fmt.Errorf("no recipe %q; configured recipes: %s", id, strings.Join(ids, ", "))
}
