package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	wordnets    = `hook=$(readlink -e "$tool/bin/hook")`
	uvSystem    = "  - { name: uv, version: 0.12.21, url: https://example.com/uv, sha256: " + sha + ", format: binary }\n"
	captainHook = "captainHook: { version: 12.88.9, url: https://example.com/captain-hook.tar.gz, sha256: " + sha + " }\n"
	captained   = systemHead + uvSystem + staticTailnet + configureEnv + captainHook
)

func newCaptainHarness(t *testing.T, layout string) (*harness, string) {
	t.Helper()
	switch layout {
	case "in place":
		return build(t, false, captained, "{}", providers.Traits{}), installs
	case "payload":
		path, archive := closureFiles(t)
		spec := fmt.Sprintf("{ payload: { path: %q, sha256: %s, packages: { path: %q, sha256: %s } } }", path, sha, archive, packagesSHA)
		return build(t, false, captained+closureApt, spec, providers.Traits{Supervisor: providers.SupervisorSpriteEnv}), loaderPhase
	case "baked image":
		h := build(t, false, imaged+"system:\n"+uvSystem+captainHook, "{ image: agent-host }", providers.Traits{Platform: images.DefaultPlatform})
		h.machine.baked = string(h.session.baked)
		return h, stages
	}
	t.Fatalf("no %s layout", layout)
	return nil, ""
}

func creator(h *harness, spare bool) func(context.Context, string, Source) (*Result, error) {
	if spare {
		return h.session.CreateSpare
	}
	return h.session.Create
}

func TestEveryCreatePreparesWordnetOnceBeforeItPublishes(t *testing.T) {
	for _, layout := range []string{"in place", "payload", "baked image"} {
		for _, spare := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spare=%v", layout, spare), func(t *testing.T) {
				h, after := newCaptainHarness(t, layout)
				if _, err := creator(h, spare)(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
					t.Fatal(err)
				}
				if ran := h.machine.ran("ws-1", wordnets); ran != 1 {
					t.Fatalf("prepared WordNet %d times in %q", ran, h.machine.scripts["ws-1"])
				}
				at := h.ordered(0, after, wordnets, publishes)[1]
				script := h.machine.scripts["ws-1"][at]
				if !strings.HasPrefix(script, "sh -c set -eu\n") || !strings.Contains(script, `"$HOME/.daemonkit/tools/capt-hook/"12.88.9`) {
					t.Errorf("WordNet did not run as the worker user against the inventory's Captain Hook tool env: %q", script)
				}
				if stdin := h.machine.stdins["ws-1"][at]; stdin != "" || strings.Contains(script, token) {
					t.Errorf("the WordNet phase received %q on stdin or carries a secret", stdin)
				}
			})
		}
	}
}

func TestAFailedWordnetPreparationFailsTheCreateBeforeItPublishes(t *testing.T) {
	for _, spare := range []bool{false, true} {
		t.Run(fmt.Sprintf("spare=%v", spare), func(t *testing.T) {
			h, _ := newCaptainHarness(t, "in place")
			held := h.machine.hold(t, wordnets)
			held.release <- providers.Result{Stderr: []byte("cc-remote: /home/fake/.wn_data/wn.db fails quick_check: database disk image is malformed"), ExitCode: 7}
			result, err := creator(h, spare)(context.Background(), "ws-1", Source{Ref: "main"})
			if result != nil || err == nil || !strings.Contains(err.Error(), "prepare WordNet: sh on ws-1 exited 7: cc-remote: /home/fake/.wn_data/wn.db fails quick_check: database disk image is malformed") {
				t.Fatalf("create = %+v, %v; want the WordNet exit and its reason", result, err)
			}
			if h.machine.ran("ws-1", wordnets) != 1 || h.machine.ran("ws-1", publishes) != 0 {
				t.Errorf("the failed create retried WordNet or published: %q", h.machine.scripts["ws-1"])
			}
			if _, found := h.record("ws-1"); found {
				t.Error("the failed create kept its workspace record")
			}
		})
	}
}

func TestResumeAndReuseLeaveWordnetToTheCreate(t *testing.T) {
	ctx := context.Background()
	for _, spare := range []bool{false, true} {
		t.Run(fmt.Sprintf("spare=%v", spare), func(t *testing.T) {
			h, _ := newCaptainHarness(t, "in place")
			if _, err := creator(h, spare)(ctx, "ws-1", Source{Ref: "main"}); err != nil {
				t.Fatal(err)
			}
			if err := h.session.Suspend(ctx, "ws-1"); err != nil {
				t.Fatal(err)
			}
			var err error
			if spare {
				_, err = h.session.Reuse(ctx, "ws-1", Source{Ref: "feature"})
			} else {
				_, err = h.session.Resume(ctx, "ws-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			if ran := h.machine.ran("ws-1", wordnets); ran != 1 {
				t.Errorf("WordNet ran %d times; a resumed or reused workspace keeps the create's", ran)
			}
		})
	}
}

func TestAnInventoryWithoutCaptainHookPreparesNoWordnet(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if ran := h.machine.ran("ws-1", wordnets); ran != 0 {
		t.Errorf("an inventory without Captain Hook prepared WordNet %d times", ran)
	}
}

func TestThePoolKeyCarriesTheWordnetContract(t *testing.T) {
	h, _ := newCaptainHarness(t, "in place")
	keyed := h.session.PoolKey()
	script := wordnetScript("12.88.9", wordnetArchiveSHA256)
	sum := sha256.Sum256([]byte(script))
	if h.session.wordnetContract() != hex.EncodeToString(sum[:]) || !strings.Contains(script, wordnetArchiveSHA256) {
		t.Errorf("contract %s does not hash the script that pins archive %s", h.session.wordnetContract(), wordnetArchiveSHA256)
	}
	h.session.captainHook = "12.88.10"
	upgraded := h.session.PoolKey()
	h.session.captainHook = ""
	if upgraded == keyed || h.session.PoolKey() == keyed || h.session.PoolKey() == upgraded || h.session.wordnetContract() != "" {
		t.Errorf("pool keys %s, %s, %s do not change with the WordNet contract", keyed, upgraded, h.session.PoolKey())
	}
}
