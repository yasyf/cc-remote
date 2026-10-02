package images

import "testing"

func TestTailnetFromToolsOnlyWhenStaticSystemArtifactsProvideBothBins(t *testing.T) {
	archive := Artifact{Name: "tailscale", Version: "1.90.0", URL: "https://example.com/tailscale.tgz", SHA256: digest, Format: TarGz, Bins: map[string]string{"tailscale": "tailscale/tailscale", "tailscaled": "tailscale/tailscaled"}}
	deb := Artifact{Name: "tailscale", Version: "1.90.0", URL: "https://example.com/tailscale.deb", SHA256: digest, Format: Deb, Bins: map[string]string{"tailscale": "/usr/bin/tailscale", "tailscaled": "/usr/sbin/tailscaled"}}
	cli := Artifact{Name: "tailscale", Version: "1.90.0", URL: "https://example.com/tailscale.tgz", SHA256: digest, Format: TarGz, Bins: map[string]string{"tailscale": "tailscale/tailscale"}}
	daemon := Artifact{Name: "tailscaled", Version: "1.90.0", URL: "https://example.com/tailscaled", SHA256: digest, Format: Binary}
	debDaemon := Artifact{Name: "tailscaled", Version: "1.90.0", URL: "https://example.com/tailscaled.deb", SHA256: digest, Format: Deb, Bins: map[string]string{"tailscaled": "/usr/sbin/tailscaled"}}
	tests := []struct {
		name   string
		system []Artifact
		tools  []Artifact
		want   bool
	}{
		{name: "a static archive providing both bins", system: []Artifact{archive}, want: true},
		{name: "the bins split across two static artifacts", system: []Artifact{cli, daemon}, want: true},
		{name: "a deb providing both bins", system: []Artifact{deb}},
		{name: "a static cli with the daemon from a deb", system: []Artifact{cli, debDaemon}},
		{name: "only the cli", system: []Artifact{cli}},
		{name: "no tailnet artifact"},
		{name: "a tools artifact rather than a system one", tools: []Artifact{archive}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := validInventory()
			inv.System = append(inv.System, tt.system...)
			inv.Tools = append(inv.Tools, tt.tools...)
			if err := inv.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := inv.TailnetFromTools(); got != tt.want {
				t.Errorf("TailnetFromTools() = %v, want %v", got, tt.want)
			}
		})
	}
}
