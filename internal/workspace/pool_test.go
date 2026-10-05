package workspace

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
)

func TestPoolKeyChangesWithWhatAWarmWorkspaceWasBuiltFrom(t *testing.T) {
	base := func() *Session {
		return &Session{
			Config:  &config.Config{Repository: "https://github.com/example/app", Roots: map[string]string{"sprites": "/home/sprite"}},
			Kind:    "sprites",
			Profile: "lean",
			Stamp:   strings.Repeat("a", 64),
			profile: config.Profile{Checkout: config.Shallow},
		}
	}
	key := base().PoolKey()
	if again := base().PoolKey(); again != key || len(key) != 64 {
		t.Fatalf("PoolKey = %q then %q, want one stable digest", key, again)
	}
	tests := []struct {
		name string
		edit func(*Session)
	}{
		{"repository", func(s *Session) { s.Config.Repository = "https://github.com/example/fork" }},
		{"provider", func(s *Session) {
			s.Kind, s.Config.Roots = "namespace", map[string]string{"namespace": "/home/sprite"}
		}},
		{"profile", func(s *Session) { s.Profile = "other" }},
		{"project root", func(s *Session) { s.Config.Roots["sprites"] = "/srv" }},
		{"checkout", func(s *Session) { s.profile.Checkout = config.Full }},
		{"tools stamp", func(s *Session) { s.Stamp = strings.Repeat("b", 64) }},
		{"image", func(s *Session) { s.image, s.imageSpec = "agent-host", strings.Repeat("c", 64) }},
		{"tailnet tag", func(s *Session) { s.Config.Tailnet = &config.Tailnet{Tag: "tag:cc-remote"} }},
		{"one prepare step", func(s *Session) { s.profile.Prepare = []string{"make deps"} }},
		{"two prepare steps", func(s *Session) { s.profile.Prepare = []string{"make", "deps"} }},
		{"bootstrap helper", func(s *Session) {
			s.Config.Orca.BootstrapHelper = &config.BootstrapHelper{Source: config.Source{URLCommand: []string{"./payload-url", "bootstrap-helper"}, SHA256: strings.Repeat("d", 64), Size: 6288446}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: strings.Repeat("e", 64)}
		}},
		{"another bootstrap helper binary", func(s *Session) {
			s.Config.Orca.BootstrapHelper = &config.BootstrapHelper{Source: config.Source{URLCommand: []string{"./payload-url", "bootstrap-helper"}, SHA256: strings.Repeat("d", 64), Size: 6288446}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: strings.Repeat("e", 64)}
			s.Config.Orca.BootstrapHelper.BinarySHA256 = strings.Repeat("f", 64)
		}},
		{"another bootstrap helper release", func(s *Session) {
			s.Config.Orca.BootstrapHelper = &config.BootstrapHelper{Source: config.Source{URLCommand: []string{"./payload-url", "bootstrap-helper"}, SHA256: strings.Repeat("d", 64), Size: 6288446}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: strings.Repeat("e", 64)}
			s.Config.Orca.BootstrapHelper.Version, s.Config.Orca.BootstrapHelper.SHA256 = "0.21.0", strings.Repeat("0", 64)
		}},
	}
	seen := map[string]string{key: "the base"}
	for _, tt := range tests {
		s := base()
		tt.edit(s)
		got := s.PoolKey()
		if other, ok := seen[got]; ok {
			t.Errorf("a different %s keeps the key of %s", tt.name, other)
		}
		seen[got] = tt.name
	}
}

func TestPoolableNeedsAShallowAgentOnlyProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile config.Profile
		want    string
	}{
		{"agent only", config.Profile{Checkout: config.Shallow}, ""},
		{"prepare steps", config.Profile{Checkout: config.Shallow, Prepare: []string{"make deps"}}, "profile lean runs prepare steps"},
		{"full checkout", config.Profile{Checkout: config.Full}, "profile lean keeps a full checkout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Session{Profile: "lean", profile: tt.profile}).Poolable()
			if (tt.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Poolable = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReuseRefreshesTheCheckoutWithoutProvisioningOrPrepareSteps(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.session.CreateSpare(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	machine, err := h.fake.Get(ctx, "ws-1")
	if err != nil || machine.Labels[LabelWorkspace] != "ws-1" || machine.Labels[LabelPool] != h.session.PoolKey() {
		t.Fatalf("machine = %+v, %v; want the workspace and pool labels", machine, err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	checkouts, prepared := h.machine.ran("ws-1", checkout), h.machine.ran("ws-1", prepares)
	before := len(h.machine.scripts["ws-1"])
	result, err := h.session.Reuse(ctx, "ws-1", Source{Ref: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	reused := strings.Join(h.machine.scripts["ws-1"][before:], "\n")
	if h.machine.ran("ws-1", checkout) != checkouts+1 || !strings.Contains(reused, "ref=feature\n") || !strings.Contains(reused, "fetch --quiet --prune --depth 1 origin") {
		t.Errorf("reuse did not check out feature shallowly: %q", reused)
	}
	if h.machine.ran("ws-1", prepares) != prepared || !strings.Contains(reused, readies) || strings.Contains(reused, installs) || strings.Contains(reused, publishes) || strings.Contains(reused, provisions) || !strings.Contains(reused, configures) {
		t.Errorf("reuse ran prepare steps, provisioned, or skipped the ready check or configure: %q", reused)
	}
	if !strings.Contains(h.calls(), "wake ws-1") {
		t.Errorf("provider calls = %s, want ws-1 woken", h.calls())
	}
	if record, ok := h.record("ws-1"); !ok || record.Source != (Source{Ref: "feature"}) || result.Source != record.Source || result.Name != "ws-1" || result.Machine != "ws-1" {
		t.Errorf("record = %+v, result = %+v; want ws-1 recorded on feature", record, result)
	}
}

func TestReuseNeverProvisionsToolsThatDriftedFromTheStamp(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.session.CreateSpare(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.NewReplacer("1.8.2", "1.8.3", sha, strings.Repeat("ab", 32)).Replace(inventory))
	if drifted.PoolKey() == h.session.PoolKey() {
		t.Error("a tools drift kept the pool key")
	}
	checkouts := h.machine.ran("ws-1", checkout)
	before := len(h.machine.scripts["ws-1"])
	if _, err := drifted.Reuse(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "never provisioned again in place") {
		t.Fatalf("Reuse = %v, want the drifted tools refused", err)
	}
	reused := strings.Join(h.machine.scripts["ws-1"][before:], "\n")
	if h.machine.ran("ws-1", checkout) != checkouts || strings.Contains(reused, installs) || strings.Contains(reused, publishes) || strings.Contains(reused, provisions) || strings.Contains(reused, configures) {
		t.Errorf("a refused reuse went on past the ready check: %q", reused)
	}
	if record, ok := h.record("ws-1"); !ok || record.Source.Ref != "main" {
		t.Errorf("record = %+v, want the source kept", record)
	}
}

func TestABootstrapHelperChangesOnlyThePoolKey(t *testing.T) {
	h := newPayloadHarness(t)
	before, key := h.session, h.session.PoolKey()
	h.cfg.Orca.BootstrapHelper = &config.BootstrapHelper{Source: config.Source{URLCommand: []string{"./payload-url", "bootstrap-helper"}, SHA256: strings.Repeat("d", 64), Size: 6288446}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: strings.Repeat("e", 64)}
	after := h.open()
	if after.PoolKey() == key {
		t.Error("a bootstrap helper kept the pool key of a workspace without one")
	}
	if after.Stamp != before.Stamp || after.Scripts.Fingerprint() != before.Scripts.Fingerprint() || !bytes.Equal(after.Scripts.ProvisionScript, before.Scripts.ProvisionScript) || !bytes.Equal(after.Scripts.Plugins, before.Scripts.Plugins) {
		t.Errorf("the helper changed the rendered tools: stamp %s -> %s, scripts %s -> %s", before.Stamp, after.Stamp, before.Scripts.Fingerprint(), after.Scripts.Fingerprint())
	}
}
