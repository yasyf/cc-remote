package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func testPregrant(project string) *orca.Pregrant {
	hooks := make([]orca.GrantedHook, 5)
	for i := range hooks {
		hooks[i] = orca.GrantedHook{Key: fmt.Sprintf("/home/agent/.codex/hooks.json:event:%d:0", i), Hash: fmt.Sprintf("sha256:%064x", i+1)}
	}
	return &orca.Pregrant{Project: project, Hooks: hooks}
}

func TestWarmClaimRefusesAPregrantTheCurrentPolicyNoLongerAuthorizes(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	w.spare(pool, "pool-a")
	member := w.member("pool-a")
	member.Pregrant = testPregrant(w.root)
	if err := pool.save(member); err != nil {
		t.Fatal(err)
	}
	if w.session.Config.Trusted() || member.Key != w.session.PoolKey() {
		t.Fatalf("the fixture must keep the pool key while orca.trust no longer lists %s", w.session.Config.Repository)
	}
	calls := len(w.provider.Calls())
	if claimed, release, err := pool.claim(t.Context(), "lane-a", "main"); err != nil || claimed != nil || release != nil {
		t.Fatalf("claim = %+v, %v; want the stale pregrant refused", claimed, err)
	}
	if got := w.member("pool-a"); got.State != memberRefused || got.Reason != "it holds a Codex trust pregrant that orca.trust no longer authorizes" || got.Lane != "" {
		t.Errorf("pool-a = %+v", got)
	}
	if after := w.provider.Calls()[calls:]; len(after) != 0 {
		t.Errorf("the refused pregrant was touched: %q", after)
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("a refusal read the key: %v", err)
	}
	w.free("pool-a")
}

type silentOrca struct{ t *testing.T }

func (s silentOrca) Run(_ context.Context, args ...string) ([]byte, error) {
	s.t.Errorf("orca was called before the pregrant was revalidated: %q", args)
	return nil, fmt.Errorf("unexpected orca call")
}

func TestAClaimedPregrantIsRevalidatedBeforeAnyKeyOrTerminal(t *testing.T) {
	tests := []struct {
		name string
		edit func(*firstWorker)
		want string
	}{
		{"its hooks cannot be verified", func(*firstWorker) {}, "selects 0 Captain Hook plugins"},
		{"its checkout left the configured origin", func(w *firstWorker) {
			gitOutput(t, w.root, "remote", "set-url", "origin", "https://github.com/someone-else/app")
		}, "the checkout's origin is not the configured repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWarmPool(t, "pool-a")
			standInFill(t)
			if _, err := openPool(w.session).fill(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := w.session.Suspend(t.Context(), "pool-a"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
			tt.edit(w)
			w.session.Config.Orca.Keys[config.KeyOpenAI] = w.session.Config.Orca.Keys[config.KeyAnthropic]
			scripts := len(w.local.Scripts("pool-a"))
			launch := orcaLaunch{agent: orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6-astra", Effort: "xhigh"}, warm: true}
			finished := false
			task, err := launch.prime(t.Context(), w.session, silentOrca{t}, orca.Runtime{Entry: "tools/orca/AppRun"}, "pool-a", openSpare, testPregrant(w.root), func(context.Context, orcaDriver, *orcaTask) error {
				finished = true
				return nil
			})
			if err == nil || task != nil || finished || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prime = %+v, %v; want %q", task, err, tt.want)
			}
			if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
				t.Errorf("the key was read before the pregrant was revalidated: %v", err)
			}
			ran := w.local.Scripts("pool-a")[scripts:]
			checked := false
			for _, script := range ran {
				checked = checked || strings.Contains(script, "status --porcelain --untracked-files=all")
				if serveCommand.MatchString(script) || strings.Contains(script, "mkfifo") {
					t.Errorf("a runtime or key pipe started before the revalidation: %q", script)
				}
			}
			if !checked {
				t.Errorf("the claimed checkout was not checked: %q", ran)
			}
			if found, err := os.Stat(w.session.Config.State().Orca("pool-a")); err == nil {
				t.Errorf("a task was recorded for a refused pregrant: %v", found)
			}
			w.free("pool-a")
		})
	}
}
