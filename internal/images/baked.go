package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	ManifestPath = "/opt/cc-remote/baked.json"
	bakedSchema  = 1
	readBaked    = "cat " + ManifestPath + " 2> /dev/null || true"
)

type Baked struct {
	Schema       int      `json:"schema"`
	Profile      string   `json:"profile"`
	Platform     string   `json:"platform"`
	Inputs       string   `json:"inputs"`
	Tools        string   `json:"tools"`
	User         string   `json:"user"`
	Home         string   `json:"home"`
	Artifacts    []string `json:"artifacts"`
	Marketplaces []string `json:"marketplaces"`
	Plugins      []string `json:"plugins"`
}

func bakedOf(inv Inventory, profile, platform, inputs string, scripts Scripts, home string) ([]byte, error) {
	baked := Baked{
		Schema:       bakedSchema,
		Profile:      profile,
		Platform:     platform,
		Inputs:       inputs,
		Tools:        scripts.Fingerprint(),
		User:         inv.Image.User,
		Home:         home,
		Artifacts:    []string{},
		Marketplaces: []string{},
		Plugins:      []string{},
	}
	for _, a := range newView(inv, profile).Tools {
		algorithm, digest := a.digest()
		baked.Artifacts = append(baked.Artifacts, a.Name+" "+a.Version+" "+algorithm+":"+digest)
	}
	if runtime := inv.CodexRuntime; runtime != nil {
		baked.Artifacts = append(baked.Artifacts, "codex-runtime "+runtime.Version+" sha256:"+runtime.SHA256)
	}
	if hook := inv.CaptainHook; hook != nil {
		baked.Artifacts = append(baked.Artifacts, "captain-hook "+hook.Version+" sha256:"+hook.SHA256)
	}
	for _, m := range inv.Claude.Marketplaces {
		pin := "ref:" + m.Ref
		if m.Ref == "" {
			pin = "branch:" + m.Branch
		}
		baked.Marketplaces = append(baked.Marketplaces, m.Name+" "+m.GitHub+" "+pin)
	}
	for _, p := range inv.Claude.Plugins {
		baked.Plugins = append(baked.Plugins, p.ID+" "+p.Version)
	}
	raw, err := json.MarshalIndent(baked, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render the baked manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

func (b Baked) String() string {
	return fmt.Sprintf("profile %q on %s with image inputs %.12s and tools %.12s for user %s", b.Profile, b.Platform, b.Inputs, b.Tools, b.User)
}

func ReadBaked(ctx context.Context, capture Capture) ([]byte, error) {
	out, err := capture(ctx, []string{"sh", "-c", readBaked}, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestPath, err)
	}
	return out, nil
}

func Adopt(want, baked []byte) error {
	if len(bytes.TrimSpace(baked)) == 0 {
		return fmt.Errorf("it carries no baked manifest at %s, so cc-remote images build did not bake it for a profile", ManifestPath)
	}
	if bytes.Equal(baked, want) {
		return nil
	}
	var have, need Baked
	if err := errors.Join(json.Unmarshal(baked, &have), json.Unmarshal(want, &need)); err != nil {
		return fmt.Errorf("its baked manifest at %s does not decode: %w", ManifestPath, err)
	}
	return fmt.Errorf("it was baked as %s, not %s", have, need)
}
