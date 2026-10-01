package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/budget"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const (
	suffix     = "example.ts.net"
	tag        = "tag:cc-remote"
	token      = "ghp_" + "testtokenthatmustnotreachascript00000000"
	enrolls    = ` up --auth-key="file:`
	freshens   = `! sudo -n test -e "/var/lib/tailscale/tailscaled.state"`
	logsOut    = "tailscale --socket=/run/tailscale/tailscaled.sock logout"
	renews     = `rm -rf "$HOME"/.claude.json`
	checkout   = "clone --quiet"
	prepares   = "cd /home/fake/app\n"
	warms      = "warm-head"
	cleans     = "this spare is not clean"
	noState    = `{"BackendState":"NoState"}`
	provisions = "sudo bash -s"
	installs   = "plugins.sh install "
	readies    = "plugins.sh ready "
	configures = "plugins.sh configure"
	inventory  = "version: 1\nsystem:\n  - { name: jq, version: 1.8.2, url: https://example.com/jq-1.8.2, sha256: " + sha + ", format: binary }\nconfigure:\n  env: [WEB_PORT]\n"
	imaged     = "version: 1\nimage:\n  name: agent-host\n  base: ubuntu:24.04@sha256:" + sha + "\n  user: agent\n  workspaceDir: /workspaces\nconfigure:\n  env: [WEB_PORT]\n"
	sha        = "008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3"
	commit     = "008173c23f95b170204355c12626cb5a965d779a"
	private    = "version: 1\nsystem:\n  - { name: claude, version: 2.0.0, url: https://example.com/claude, sha256: " + sha + ", format: binary }\nclaude:\n  marketplaces:\n    - { name: market, github: owner/market, ref: " + commit + ", private: true }\nconfigure:\n  env: [WEB_PORT]\n"
)

func running(nodeID string) string {
	return `{"BackendState":"Running","Self":{"ID":"` + nodeID + `","DNSName":"ws-1.` + suffix + `.","TailscaleIPs":["100.64.0.9"]}}`
}

type scripted struct {
	mu       sync.Mutex
	status   string
	minted   string
	fail     error
	failAll  bool
	onEnroll func()
	onStatus func()
	scripts  map[string][]string
	stdins   map[string][]string
	ready    map[string]string
}

func (m *scripted) setStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
}

func (m *scripted) currentStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *scripted) Handle(id string, cmd []string, stdin []byte) providers.Result {
	script := strings.Join(cmd, " ")
	m.mu.Lock()
	if m.scripts == nil {
		m.scripts, m.stdins, m.ready = map[string][]string{}, map[string][]string{}, map[string]string{}
	}
	m.scripts[id] = append(m.scripts[id], script)
	m.stdins[id] = append(m.stdins[id], strings.TrimSpace(string(stdin)))
	failAll := m.failAll
	stamp := cmd[len(cmd)-1]
	switch {
	case strings.Contains(script, installs):
		m.ready[id] = stamp
	case strings.Contains(script, readies) && m.ready[id] != stamp:
		m.mu.Unlock()
		return providers.Result{Stderr: []byte("plugins: this host was not prepared from stamp " + stamp), ExitCode: 1}
	}
	m.mu.Unlock()
	if failAll {
		return providers.Result{Stderr: []byte("the machine went away"), ExitCode: 1}
	}
	switch {
	case strings.Contains(script, enrolls):
		if m.onEnroll != nil {
			m.onEnroll()
		}
		if m.fail != nil {
			return providers.Result{Stderr: []byte(m.fail.Error()), ExitCode: 1}
		}
		m.setStatus(running(m.minted))
		return providers.Result{Stdout: []byte(m.currentStatus())}
	case strings.Contains(script, "status --json"):
		if m.onStatus != nil {
			m.onStatus()
		}
		return providers.Result{Stdout: []byte(m.currentStatus())}
	case strings.Contains(script, logsOut):
		m.setStatus(noState)
	}
	return providers.Result{}
}

func (m *scripted) lose(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ready, id)
}

func (m *scripted) ran(id, fragment string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, script := range m.scripts[id] {
		if strings.Contains(script, fragment) {
			count++
		}
	}
	return count
}

func (m *scripted) order(id string, from int, fragments ...string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	positions := make([]int, len(fragments))
	for i, fragment := range fragments {
		positions[i] = -1
		for j := from; j < len(m.scripts[id]); j++ {
			if strings.Contains(m.scripts[id][j], fragment) {
				positions[i] = j
				break
			}
		}
	}
	return positions
}

func verbs(calls []string) string {
	words := make([]string, 0, len(calls))
	for _, call := range calls {
		words = append(words, strings.Fields(call)[0])
	}
	return strings.Join(words, " ")
}

type fakeTailnet struct {
	mu      sync.Mutex
	devices []tailnet.Device
	refuse  bool
	minted  int
	deleted []string
}

func (f *fakeTailnet) serve(t *testing.T) *tailnet.Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"t"}`)
		case r.URL.Path == "/tailnet/-/keys":
			f.minted++
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "tskey-auth-minted"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/device/"):
			for _, device := range f.devices {
				if device.NodeID == strings.TrimPrefix(r.URL.Path, "/device/") {
					_ = json.NewEncoder(w).Encode(device)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodDelete:
			if f.refuse {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/device/"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return &tailnet.Client{Tag: tag, Base: server.URL, HTTP: server.Client(), Credential: tailnet.Credential{ClientID: "k", ClientSecret: "s", Suffix: suffix}}
}

func (f *fakeTailnet) deletedNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeTailnet) mintedKeys() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted
}

func owned(nodeID string) tailnet.Device {
	return tailnet.Device{NodeID: nodeID, Name: "ws-1." + suffix, Hostname: "ws-1", Tags: []string{tag}}
}

type flaky struct {
	providers.Provider
	unreachable atomic.Bool
	onSSH       func()
}

func (f *flaky) SSHTarget(ctx context.Context, id string) (providers.Target, error) {
	if f.unreachable.Load() {
		return providers.Target{}, errors.New("no route to " + id)
	}
	if f.onSSH != nil {
		f.onSSH()
	}
	return f.Provider.SSHTarget(ctx, id)
}

type harness struct {
	t         *testing.T
	session   *Session
	fake      *providertest.Fake
	provider  *flaky
	machine   *scripted
	api       *fakeTailnet
	cfg       *config.Config
	inventory string
}

func newHarness(t *testing.T, spares int, withTailnet bool) *harness {
	t.Helper()
	return build(t, spares, withTailnet, inventory, "")
}

func newImagedHarness(t *testing.T) *harness {
	t.Helper()
	return build(t, 0, false, imaged, "agent-host")
}

func build(t *testing.T, spares int, withTailnet bool, tools, image string) *harness {
	t.Helper()
	machine := &scripted{status: noState, minted: "nNEW"}
	fake := &providertest.Fake{Rates: providers.Rate{HourlyUSD: 1}, Handle: machine.Handle}
	provider := &flaky{Provider: fake}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(inventoryPath, []byte(tools), 0o600); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`
repository: https://github.com/example/app
ref: main
provider: fake
profile: lean
state_dir: %s
providers:
  fake: {}
workspace_dirs:
  fake: /home/fake
profiles:
  lean:
    prepare: [true]
    warm: ["yarn install"]
    warm_inputs: [yarn.lock]
    machine:
      fake: { image: %q }
spares:
  fake: { lean: %d }
inventory: %s
forwards:
  - { label: web, env: WEB_PORT }
budget:
  cap_usd: 100
  reserve_usd: 10
  trial_hours: 1
`, t.TempDir(), image, spares, inventoryPath)
	if withTailnet {
		text += "tailnet:\n  tag: " + tag + "\n"
	}
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	h := &harness{t: t, fake: fake, provider: provider, machine: machine, cfg: cfg, inventory: inventoryPath}
	h.session = h.open()
	if err := h.session.Pool.Ledger.Init(time.Now()); err != nil {
		t.Fatal(err)
	}
	h.api = &fakeTailnet{devices: []tailnet.Device{owned("nNEW")}}
	if withTailnet {
		client := h.api.serve(t)
		h.session.Enroller.Connect = func(context.Context) (*tailnet.Client, error) { return client, nil }
		h.session.Enroller.Log = h.session.Log
	}
	return h
}

func (h *harness) open() *Session {
	h.t.Helper()
	session, err := Open(h.cfg, h.provider, "fake", "lean", Platform{Daemon: tailnet.Daemon{Mode: tailnet.Kernel, Supervisor: tailnet.SpriteEnv}, HostKeys: true})
	if err != nil {
		h.t.Fatal(err)
	}
	session.Log = slog.New(slog.DiscardHandler)
	session.Stderr = io.Discard
	session.Token = func(context.Context) (string, error) { return token, nil }
	return session
}

func (h *harness) drift(tools string) *Session {
	h.t.Helper()
	if err := os.WriteFile(h.inventory, []byte(tools), 0o600); err != nil {
		h.t.Fatal(err)
	}
	drifted := h.open()
	if drifted.Stamp == h.session.Stamp {
		h.t.Fatal("the inventory drift left the stamp unchanged")
	}
	return drifted
}

func (h *harness) bound() tailnet.Binding {
	h.t.Helper()
	bindings := tailnet.Bindings{Dir: h.session.State.Tailnet()}
	binding, err := bindings.Read("ws-1")
	if err != nil {
		h.t.Fatal(err)
	}
	return binding
}

func (h *harness) bind(to tailnet.Binding) {
	h.t.Helper()
	held, err := h.session.State.Hold("ws-1")
	if err != nil {
		h.t.Fatal(err)
	}
	defer held.Release()
	bindings := tailnet.Bindings{Dir: h.session.State.Tailnet()}
	if err := bindings.Of(held).Transition(tailnet.Binding{}, to); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ledger() *budget.Ledger {
	h.t.Helper()
	ledger, err := h.session.Pool.Ledger.Read()
	if err != nil {
		h.t.Fatal(err)
	}
	return ledger
}

func (h *harness) spares() budget.Spares {
	h.t.Helper()
	spares, err := h.session.Pool.Ledger.ReadSpares()
	if err != nil {
		h.t.Fatal(err)
	}
	spares.DropRetired(h.ledger())
	return spares
}

func (h *harness) record(name string) (Record, bool) {
	h.t.Helper()
	var record Record
	found, err := state.Load(h.session.State.Workspace(name), &record)
	if err != nil {
		h.t.Fatal(err)
	}
	return record, found
}

func (h *harness) saveRecord(record Record) {
	h.t.Helper()
	if err := state.Save(h.session.State.Workspace(record.Name), record); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) prepared() string {
	h.t.Helper()
	if err := h.session.Prepare(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	for id := range h.spares() {
		return id
	}
	h.t.Fatal("prepare left no spare")
	return ""
}

func (h *harness) calls() string {
	return strings.Join(h.fake.Calls(), " ")
}

func TestAFreshCreateChecksOutPreparesBootstrapsThenEnrolls(t *testing.T) {
	h := newHarness(t, 0, true)
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	order := h.machine.order("ws-1", 0, provisions, installs, checkout, prepares, configures, freshens, enrolls)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("scripts ran out of order: %v", order)
		}
	}
	if h.machine.ran("ws-1", renews) != 0 || h.machine.ran("ws-1", warms) != 0 || h.machine.ran("ws-1", readies) != 0 {
		t.Error("a fresh create renewed identity, warmed, or checked a ready stamp it could not have")
	}
	if got := h.machine.stdins["ws-1"][order[1]]; got != "" {
		t.Errorf("the tool install read %q on stdin although no private marketplace needs a token", got)
	}
	order = order[2:]
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, token) || strings.Contains(script, "tskey-auth") {
			t.Errorf("script %d carries a secret", i)
		}
	}
	if got := h.machine.stdins["ws-1"][order[0]]; got != token {
		t.Errorf("checkout read %q on stdin", got)
	}
	if !strings.Contains(h.machine.scripts["ws-1"][order[0]], "--depth 1") || !strings.Contains(h.machine.scripts["ws-1"][order[0]], "ref=feature") {
		t.Error("the lean checkout is not a shallow checkout of the requested ref")
	}
	if result.Tailnet == nil || result.Tailnet.NodeID != "nNEW" || result.SSH.Host != "ws-1" || len(result.Forwards) != 1 || result.Source.Ref != "feature" {
		t.Errorf("result = %+v", result)
	}
	if record, found := h.record("ws-1"); !found || record.Tailnet == nil || record.Tailnet.NodeID != "nNEW" || record.Claimed {
		t.Errorf("record = %+v, %v", record, found)
	}
	if !h.ledger().Running("ws-1") || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) {
		t.Errorf("ledger running %v, bound %v", h.ledger().Running("ws-1"), h.bound())
	}
	machine, _ := h.fake.Get(context.Background(), "ws-1")
	if machine.Labels[LabelWorkspace] != "ws-1" || machine.Labels[LabelProfile] != "lean" {
		t.Errorf("labels = %v", machine.Labels)
	}
}

func TestAClaimRenewsIdentityBeforeInstallingThenWarmsAndEnrolls(t *testing.T) {
	h := newHarness(t, 1, true)
	spare := h.prepared()
	if h.machine.ran(spare, enrolls) != 0 || h.machine.ran(spare, freshens) != 0 || h.machine.ran(spare, cleans) != 1 || h.machine.ran(spare, warms) != 1 {
		t.Errorf("prepare ran %q", h.machine.scripts[spare])
	}
	if machine, _ := h.fake.Get(context.Background(), spare); machine.State != providers.StateSuspended || machine.Labels[LabelSpare] != h.session.Pool.Fingerprint {
		t.Errorf("the ready spare is %+v", machine)
	}
	refills := 0
	h.session.Refill = func() error { refills++; return nil }
	before := len(h.machine.scripts[spare])
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	order := h.machine.order(spare, before, renews, readies, checkout, prepares, warms, configures, freshens, enrolls)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("the claim ran scripts out of order: %v (prepare ran %d)", order, before)
		}
	}
	if from := h.machine.scripts[spare][before:]; strings.Contains(strings.Join(from, "\n"), installs) || strings.Contains(strings.Join(from, "\n"), provisions) {
		t.Errorf("a claim of a spare at the current stamp installed tools again: %q", from)
	}
	if fresh := h.machine.scripts[spare][order[0]]; !strings.Contains(fresh, "sudo -n ssh-keygen -A") {
		t.Errorf("the claim kept the spare's host keys: %q", fresh)
	}
	if refills != 1 || result.Machine != spare || !strings.Contains(h.calls(), "wake "+spare) {
		t.Errorf("refills %d, machine %s, calls %s", refills, result.Machine, h.calls())
	}
	if spare := h.spares()[spare]; spare.State != budget.Claimed || spare.ActivatedAt == nil || spare.Request != "ws-1" {
		t.Errorf("spare = %+v", spare)
	}
	if record, found := h.record("ws-1"); !found || !record.Claimed || record.Machine != spare || record.Tailnet == nil {
		t.Errorf("record = %+v, %v", record, found)
	}
}

func TestARetriedCreateReattachesItsClaimWithoutRenewingIdentity(t *testing.T) {
	h := newHarness(t, 1, true)
	spare := h.prepared()
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	identities, minted := h.machine.ran(spare, renews), h.api.mintedKeys()
	calls := len(h.fake.Calls())
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if h.machine.ran(spare, renews) != identities || h.api.mintedKeys() != minted || h.machine.ran(spare, checkout) != 2 {
		t.Error("the retry renewed the identity of a workspace already handed out, or enrolled again")
	}
	if got := verbs(h.fake.Calls()[calls:]); strings.Contains(got, "create") || strings.Contains(got, "destroy") || !strings.HasPrefix(got, "wake exec") {
		t.Errorf("the retry ran %q; want a wake, a refresh and a reconnect only", got)
	}
	if h.spares()[spare].State != budget.Claimed {
		t.Errorf("the reattached spare is %s", h.spares()[spare].State)
	}
}

func TestAFailedReattachLeavesTheWorkspace(t *testing.T) {
	h := newHarness(t, 1, true)
	spare := h.prepared()
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	h.machine.failAll = true
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil {
		t.Fatal("the reattach succeeded on a machine that fails every command")
	}
	if strings.Contains(h.calls(), "destroy") {
		t.Error("a failed reattach destroyed the workspace it was handed")
	}
	if h.ledger().Resources[spare].Destroyed != nil {
		t.Error("a failed reattach retired the workspace")
	}
	if _, found := h.record("ws-1"); !found {
		t.Error("a failed reattach forgot the workspace")
	}
}

func TestARetryOfAnUnfinishedCreateTouchesNothing(t *testing.T) {
	h := newHarness(t, 1, false)
	spare := h.prepared()
	if _, assignment, err := h.session.Pool.Assign("ws-1", time.Now()); err != nil || assignment != NewClaim {
		t.Fatal(assignment, err)
	}
	calls := len(h.fake.Calls())
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "never finished") {
		t.Fatalf("a retry reattached a claim whose first create never finished: %v", err)
	}
	if len(h.fake.Calls()) != calls {
		t.Errorf("the refused retry ran %v on the machine", h.fake.Calls()[calls:])
	}
	if h.spares()[spare].State != budget.Claimed {
		t.Error("the refused retry changed the claim")
	}
}

func TestARetriedFreshCreateLeavesTheWorkspaceItMade(t *testing.T) {
	for name, spares := range map[string]int{"pooled": 1, "unpooled": 0} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, spares, false)
			if err := h.session.Pool.Start("ws-1", time.Now()); err != nil {
				t.Fatal(err)
			}
			h.machine.failAll = true
			if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("the retry of a fresh create returned %v; want a refusal before any machine call", err)
			}
			if len(h.fake.Calls()) != 0 {
				t.Errorf("the refused retry ran %v on the machine", h.fake.Calls())
			}
			if resource := h.ledger().Resources["ws-1"]; resource.Destroyed != nil || len(resource.Running) != 1 {
				t.Errorf("the refused retry changed the workspace's ledger entry: %+v", resource)
			}
		})
	}
}

func TestAFailedCreateLeavesTheTailnetThenDiscardsWhatItMade(t *testing.T) {
	h := newHarness(t, 0, true)
	h.provider.unreachable.Store(true)
	_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("err = %v", err)
	}
	if deleted := h.api.deletedNodes(); len(deleted) != 1 || deleted[0] != "nNEW" || h.machine.ran("ws-1", logsOut) != 1 || h.bound() != (tailnet.Binding{}) {
		t.Errorf("deleted %v, logged out %d, bound %v", deleted, h.machine.ran("ws-1", logsOut), h.bound())
	}
	if _, err := h.fake.Get(context.Background(), "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the machine survived the failed create: %v", err)
	}
	if resource := h.ledger().Resources["ws-1"]; resource == nil || resource.Destroyed == nil {
		t.Errorf("the ledger did not retire the failed create: %+v", resource)
	}
	if _, found := h.record("ws-1"); found {
		t.Error("the failed create left a record")
	}
}

func TestAFailedCreateDestroysTheMachineEvenWhenItsNodeCannotBeRevoked(t *testing.T) {
	h := newHarness(t, 0, true)
	h.api.refuse = true
	h.provider.unreachable.Store(true)
	_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.fake.Get(context.Background(), "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Error("the machine survived")
	}
	if len(h.api.deletedNodes()) != 0 || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) {
		t.Errorf("deleted %v, bound %v", h.api.deletedNodes(), h.bound())
	}
}

func TestAFailedResumeRevokesOnlyTheNodeItEnrolled(t *testing.T) {
	tests := map[string]struct {
		binding tailnet.Binding
		status  string
		deleted []string
		bound   tailnet.Binding
	}{
		"the live node survives a connection failure": {tailnet.Binding{NodeID: "nMINE"}, running("nMINE"), nil, tailnet.Binding{NodeID: "nMINE"}},
		"the node this resume enrolled is revoked":    {tailnet.Binding{}, noState, []string{"nNEW"}, tailnet.Binding{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 0, true)
			h.api.devices = []tailnet.Device{owned("nMINE"), owned("nNEW")}
			if _, err := h.fake.Create(context.Background(), providers.Spec{Name: "ws-1"}); err != nil {
				t.Fatal(err)
			}
			h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Source: Source{Ref: "main"}, Machine: "ws-1"})
			h.machine.setStatus(tt.status)
			if tt.binding != (tailnet.Binding{}) {
				h.bind(tt.binding)
			}
			h.provider.unreachable.Store(true)
			if _, err := h.session.Resume(context.Background(), "ws-1"); err == nil {
				t.Fatal("the resume succeeded")
			}
			if strings.Join(h.api.deletedNodes(), ",") != strings.Join(tt.deleted, ",") || h.machine.ran("ws-1", logsOut) != len(tt.deleted) || h.bound() != tt.bound {
				t.Errorf("deleted %v, ran %q, bound %v", h.api.deletedNodes(), h.machine.scripts["ws-1"], h.bound())
			}
		})
	}
}

func TestAConcurrentResumeNeverRevokesTheNodeAnotherEstablished(t *testing.T) {
	h := newHarness(t, 0, true)
	if _, err := h.fake.Create(context.Background(), providers.Spec{Name: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Source: Source{Ref: "main"}, Machine: "ws-1"})
	enrolling, enrolled, connecting, connected := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firstEnroll, firstConnect sync.Once
	h.machine.onEnroll = func() {
		firstEnroll.Do(func() {
			close(enrolling)
			<-enrolled
		})
	}
	h.provider.onSSH = func() {
		firstConnect.Do(func() {
			close(connecting)
			<-connected
			h.provider.unreachable.Store(true)
		})
	}
	established := make(chan error, 1)
	go func() {
		_, err := h.session.Resume(context.Background(), "ws-1")
		established <- err
	}()
	<-enrolling
	retried := make(chan error, 1)
	go func() {
		_, err := h.session.Resume(context.Background(), "ws-1")
		retried <- err
	}()
	stillHeld := func(stage string) {
		select {
		case err := <-retried:
			t.Fatalf("the retry ran while the first resume was still %s: %v", stage, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	stillHeld("enrolling")
	close(enrolled)
	<-connecting
	stillHeld("connecting")
	close(connected)
	if err := <-established; err != nil {
		t.Fatal(err)
	}
	if err := <-retried; err == nil {
		t.Fatal("the retry reached a machine with no route")
	}
	if h.api.mintedKeys() != 1 || len(h.api.deletedNodes()) != 0 || h.machine.ran("ws-1", logsOut) != 0 || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) {
		t.Errorf("minted %d, deleted %v, ran %q, bound %v", h.api.mintedKeys(), h.api.deletedNodes(), h.machine.scripts["ws-1"], h.bound())
	}
}

func TestAWorkspaceWithNoTailnetNeverTouchesTheTailnet(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.session.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if h.machine.ran("ws-1", "tailscale") != 0 {
		t.Errorf("a workspace without a tailnet ran %q", h.machine.scripts["ws-1"])
	}
	if _, err := os.Stat(h.session.State.Tailnet()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a workspace without a tailnet left binding state behind: %v", err)
	}
}

func TestDestroyLeavesTheTailnetBeforeTheMachineAndRetiresTheLedger(t *testing.T) {
	h := newHarness(t, 0, true)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	calls := len(h.fake.Calls())
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	after := h.fake.Calls()[calls:]
	logout, destroy := -1, -1
	for i, call := range after {
		if strings.HasPrefix(call, "exec ws-1") && strings.Contains(call, "logout") && logout < 0 {
			logout = i
		}
		if call == "destroy ws-1" {
			destroy = i
		}
	}
	if logout < 0 || destroy < 0 || logout > destroy {
		t.Errorf("destroy ran %q; want the logout before the machine goes", after)
	}
	if deleted := h.api.deletedNodes(); len(deleted) != 1 || deleted[0] != "nNEW" || h.bound() != (tailnet.Binding{}) {
		t.Errorf("deleted %v, bound %v", deleted, h.bound())
	}
	if resource := h.ledger().Resources["ws-1"]; resource.Destroyed == nil {
		t.Error("the ledger still holds the destroyed workspace")
	}
	if _, found := h.record("ws-1"); found {
		t.Error("the record survived destroy")
	}
	if err := h.session.Destroy(ctx, "ws-1"); err == nil {
		t.Error("a second destroy found a workspace")
	}
}

func TestSuspendStopsTheLedgerAndResumeRestartsIt(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if h.ledger().Running("ws-1") || !strings.Contains(h.calls(), "suspend ws-1") {
		t.Error("suspend left the workspace running")
	}
	checkouts := h.machine.ran("ws-1", checkout)
	before := len(h.machine.scripts["ws-1"])
	result, err := h.session.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if !h.ledger().Running("ws-1") || h.machine.ran("ws-1", checkout) != checkouts || h.machine.ran("ws-1", prepares) < 2 {
		t.Error("resume did not restart the ledger, or re-cloned, or skipped the prepare steps")
	}
	if resumed := strings.Join(h.machine.scripts["ws-1"][before:], "\n"); !strings.Contains(resumed, readies) || strings.Contains(resumed, installs) || !strings.Contains(resumed, configures) {
		t.Errorf("resume did not check the ready stamp, or reinstalled, or skipped configure: %q", resumed)
	}
	if result.Forwards[0].Port == 0 {
		t.Error("resume lost the forwarded port")
	}
}

func TestResumeRefusesAnUnknownOrForeignWorkspace(t *testing.T) {
	h := newHarness(t, 0, false)
	if _, err := h.session.Resume(context.Background(), "ws-1"); err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Errorf("err = %v", err)
	}
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "full", Machine: "ws-1"})
	if _, err := h.session.Resume(context.Background(), "ws-1"); err == nil || !strings.Contains(err.Error(), "fake/full") {
		t.Errorf("err = %v", err)
	}
}

func TestAClaimFromAPoolKeptAtZeroStartsNoRefill(t *testing.T) {
	h := newHarness(t, 0, false)
	refills := 0
	h.session.Refill = func() error { refills++; return nil }
	h.session.replaceClaimed()
	if refills != 0 {
		t.Error("a pool kept at zero spares started a refill")
	}
}

func TestPrepareRefusesASpareTheBudgetCannotAdmit(t *testing.T) {
	h := newHarness(t, 5, false)
	h.session.Pool.Guard.CapUSD = 10.5
	err := h.session.Prepare(context.Background())
	var over *budget.OverBudgetError
	if !errors.As(err, &over) {
		t.Fatalf("err = %v", err)
	}
	if len(h.spares()) != 0 || len(h.fake.Calls()) != 0 {
		t.Errorf("the pool holds %d spares and the provider ran %v against a $0.50 remainder", len(h.spares()), h.fake.Calls())
	}
}

func TestAReadySpareReleasesItsReservation(t *testing.T) {
	h := newHarness(t, 2, false)
	h.session.Pool.Guard.CapUSD = 11.5
	if err := h.session.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.spares()) != 2 {
		t.Errorf("the pool holds %d spares; a ready spare accrues storage only, so the second fits once the first is ready", len(h.spares()))
	}
}

func TestAFailedPrepareDiscardsTheSpare(t *testing.T) {
	h := newHarness(t, 1, false)
	h.machine.failAll = true
	if err := h.session.Prepare(context.Background()); err == nil {
		t.Fatal("prepare passed on a machine that fails every command")
	}
	if len(h.spares()) != 0 || !strings.Contains(h.calls(), "destroy cc-remote-spare-lean-") {
		t.Errorf("the failed spare stayed: %v, %s", h.spares(), h.calls())
	}
}

func TestStatusReportsWorkspacesPoolAndBudget(t *testing.T) {
	h := newHarness(t, 1, false)
	spare := h.prepared()
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	status, err := h.session.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Workspaces) != 1 || status.Workspaces[0].Name != "ws-1" || status.Workspaces[0].Machine != spare {
		t.Errorf("workspaces = %+v", status.Workspaces)
	}
	if status.Pool.Target != 1 || status.Pool.Pooled != 0 || status.Pool.Spares[spare] == nil || status.Pool.Fingerprint != h.session.Pool.Fingerprint {
		t.Errorf("pool = %+v", status.Pool)
	}
	if status.Budget.CapUSD != 100 || status.Budget.RemainingUSD >= 90 || len(status.Resources) != 1 || !status.Resources[0].Running {
		t.Errorf("budget = %+v, resources = %+v", status.Budget, status.Resources)
	}
}

func TestAFreshCreateNeverDestroysAMachineItDidNotMake(t *testing.T) {
	h := newHarness(t, 0, true)
	ctx := context.Background()
	if _, err := h.fake.Create(ctx, providers.Spec{Name: "ws-1", Labels: map[string]string{LabelWorkspace: "someone-else"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); !errors.Is(err, providers.ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if machine, err := h.fake.Get(ctx, "ws-1"); err != nil || machine.Labels[LabelWorkspace] != "someone-else" {
		t.Errorf("the collision destroyed the other owner's machine: %+v, %v", machine, err)
	}
	if h.machine.scripts["ws-1"] != nil || h.api.mintedKeys() != 0 || len(h.api.deletedNodes()) != 0 {
		t.Errorf("the refused create ran %q, minted %d, deleted %v", h.machine.scripts["ws-1"], h.api.mintedKeys(), h.api.deletedNodes())
	}
	if _, found := h.record("ws-1"); found {
		t.Error("the refused create left a record")
	}
	if resource := h.ledger().Resources["ws-1"]; resource == nil || resource.Destroyed == nil {
		t.Errorf("the ledger still runs the machine that was never made: %+v", resource)
	}
}

func TestCreateRefusesANameAnotherLedgerRecorded(t *testing.T) {
	h := newHarness(t, 0, true)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	other := budget.Store{Path: filepath.Join(t.TempDir(), "other.json")}
	if err := other.Init(time.Now()); err != nil {
		t.Fatal(err)
	}
	h.session.Pool.Ledger = other
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.fake.Get(ctx, "ws-1"); err != nil {
		t.Error("the refused create destroyed the recorded workspace's machine")
	}
	if len(h.api.deletedNodes()) != 0 || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) {
		t.Errorf("the refused create revoked the recorded workspace's node: deleted %v, bound %v", h.api.deletedNodes(), h.bound())
	}
	if _, found := h.record("ws-1"); !found {
		t.Error("the refused create forgot the recorded workspace")
	}
}

func TestDestroyReleasesAClaimWhoseCreateNeverRecordedIt(t *testing.T) {
	h := newHarness(t, 1, false)
	ctx := context.Background()
	spare := h.prepared()
	if _, assignment, err := h.session.Pool.Assign("ws-1", time.Now()); err != nil || assignment != NewClaim {
		t.Fatal(assignment, err)
	}
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "never finished") {
		t.Fatalf("err = %v", err)
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.fake.Get(ctx, spare); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the claimed spare survived: %v", err)
	}
	if _, held := h.spares()[spare]; held {
		t.Error("the claim survived destroy")
	}
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Errorf("the name could not be created after the orphaned claim was destroyed: %v", err)
	}
}

func TestDestroyHoldsTheResourceUntilTheMachineIsGone(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	destroying, release := make(chan struct{}), make(chan struct{})
	h.session.Provider = &slowDestroy{Provider: h.provider, started: destroying, release: release}
	done := make(chan error, 1)
	go func() { done <- h.session.Destroy(ctx, "ws-1") }()
	<-destroying
	acquired := make(chan bool, 1)
	go func() {
		held, err := h.session.State.Hold("ws-1")
		if err != nil {
			t.Error(err)
		}
		defer held.Release()
		_, found := h.record("ws-1")
		acquired <- found
	}()
	select {
	case <-acquired:
		t.Fatal("the resource lock was free while the provider was still destroying the machine")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if found := <-acquired; found {
		t.Error("the lock was acquired while the destroyed workspace was still recorded")
	}
}

type slowDestroy struct {
	providers.Provider
	started chan struct{}
	release chan struct{}
}

func (s *slowDestroy) Destroy(ctx context.Context, id string) error {
	close(s.started)
	<-s.release
	return s.Provider.Destroy(ctx, id)
}

func TestPrepareNeverDestroysAMachineItDidNotMake(t *testing.T) {
	h := newHarness(t, 1, false)
	ctx := context.Background()
	h.session.Provider = &collidingCreate{Provider: h.provider}
	if err := h.session.Prepare(ctx); !errors.Is(err, providers.ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if calls := h.calls(); strings.Contains(calls, "destroy") || strings.Contains(calls, "exec") {
		t.Errorf("the refused prepare ran %s", calls)
	}
	if len(h.spares()) != 0 {
		t.Errorf("the refused prepare left %v", h.spares())
	}
	for id, resource := range h.ledger().Resources {
		if resource.Destroyed == nil {
			t.Errorf("the ledger still runs %s", id)
		}
	}
}

type collidingCreate struct {
	providers.Provider
}

func (c *collidingCreate) Create(context.Context, providers.Spec) (providers.Machine, error) {
	return providers.Machine{}, providers.ErrExists
}

func TestAFailedCreateKeepsItsRecordWhenTheMachineCannotBeRemoved(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	h.provider.unreachable.Store(true)
	gone := newRefusingDestroy(h.provider)
	h.session.Provider = gone
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "kept so that destroy can retry") {
		t.Fatalf("err = %v", err)
	}
	if _, found := h.record("ws-1"); !found {
		t.Fatal("the record was forgotten while the machine survived")
	}
	gone.refuse.Store(false)
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.fake.Get(ctx, "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Error("the machine survived the retried destroy")
	}
}

type refusingDestroy struct {
	providers.Provider
	refuse atomic.Bool
}

func (r *refusingDestroy) Destroy(ctx context.Context, id string) error {
	if r.refuse.Load() {
		return errors.New("the provider api is down")
	}
	return r.Provider.Destroy(ctx, id)
}

func newRefusingDestroy(p providers.Provider) *refusingDestroy {
	r := &refusingDestroy{Provider: p}
	r.refuse.Store(true)
	return r
}

func TestDestroyRefusesAClaimAnotherProviderOrProfileHolds(t *testing.T) {
	h := newHarness(t, 1, false)
	ctx := context.Background()
	spare := h.prepared()
	if _, assignment, err := h.session.Pool.Assign("ws-1", time.Now()); err != nil || assignment != NewClaim {
		t.Fatal(assignment, err)
	}
	other := *h.session
	other.Profile = "full"
	if err := other.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "fake/lean") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.fake.Get(ctx, spare); err != nil {
		t.Error("the other profile destroyed the claimed spare")
	}
	if _, held := h.spares()[spare]; !held {
		t.Error("the other profile released the claim")
	}
}

func TestARefusedCreateConsumesNoSpareOrBudget(t *testing.T) {
	h := newHarness(t, 1, false)
	ctx := context.Background()
	spare := h.prepared()
	calls := len(h.fake.Calls())
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Source: Source{Ref: "main"}, Machine: "ws-1"})
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("err = %v", err)
	}
	if got := h.spares()[spare]; got == nil || got.State != budget.Ready {
		t.Errorf("the refused create changed the spare: %+v", got)
	}
	if h.ledger().Running("ws-1") || len(h.fake.Calls()) != calls {
		t.Errorf("the refused create reserved budget or touched a machine: %v", h.fake.Calls()[calls:])
	}
}

func TestCreateRefusesANameStillBoundToATailnetNodeAndDestroyRevokesIt(t *testing.T) {
	h := newHarness(t, 0, true)
	ctx := context.Background()
	h.api.devices = append(h.api.devices, owned("nOLD"))
	h.bind(tailnet.Binding{NodeID: "nOLD"})
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "still bound") {
		t.Fatalf("err = %v", err)
	}
	if len(h.fake.Calls()) != 0 || len(h.api.deletedNodes()) != 0 || h.ledger().Running("ws-1") {
		t.Errorf("the refused create ran %v, deleted %v", h.fake.Calls(), h.api.deletedNodes())
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if deleted := h.api.deletedNodes(); len(deleted) != 1 || deleted[0] != "nOLD" || h.bound() != (tailnet.Binding{}) {
		t.Errorf("deleted %v, bound %v", deleted, h.bound())
	}
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Errorf("create after the revoke: %v", err)
	}
}

func TestAClaimProvisionsASpareThatLostItsReadyStamp(t *testing.T) {
	h := newHarness(t, 1, false)
	spare := h.prepared()
	h.machine.lose(spare)
	before := len(h.machine.scripts[spare])
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	order := h.machine.order(spare, before, readies, provisions, installs, checkout)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("the claim ran scripts out of order: %v", order)
		}
	}
	if h.machine.ready[spare] != h.session.Stamp {
		t.Errorf("the claim left the spare at stamp %q", h.machine.ready[spare])
	}
}

func TestTheInstallReadsTheTokenOnlyForPrivateMarketplaces(t *testing.T) {
	h := build(t, 0, false, private, "")
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	order := h.machine.order("ws-1", 0, installs)
	if order[0] < 0 || h.machine.stdins["ws-1"][order[0]] != token {
		t.Errorf("the install read %q on stdin; want the token for the private marketplace", h.machine.stdins["ws-1"][order[0]])
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, token) {
			t.Errorf("script %d carries the token", i)
		}
	}
}

func TestResumeRefusesToReprovisionAHostEnrolledInKernelMode(t *testing.T) {
	h := newHarness(t, 0, true)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.NewReplacer("1.8.2", "1.8.3", sha, strings.Repeat("ab", 32)).Replace(inventory))
	drifted.Enroller.Connect = h.session.Enroller.Connect
	before := len(h.machine.scripts["ws-1"])
	if _, err := drifted.Resume(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "already joined the tailnet") {
		t.Fatalf("Resume = %v", err)
	}
	if after := strings.Join(h.machine.scripts["ws-1"][before:], "\n"); !strings.Contains(after, readies) || strings.Contains(after, provisions) || strings.Contains(after, installs) {
		t.Errorf("the refused resume ran %q", after)
	}
}

func TestResumeProvisionsInPlaceWhenTheInventoryDrifted(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.NewReplacer("1.8.2", "1.8.3", sha, strings.Repeat("ab", 32)).Replace(inventory))
	before := len(h.machine.scripts["ws-1"])
	if _, err := drifted.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	order := h.machine.order("ws-1", before, readies, provisions, "plugins.sh.tmp", installs, prepares, configures)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("the resume ran scripts out of order: %v", order)
		}
	}
	provision, plugins := h.machine.stdins["ws-1"][order[1]], h.machine.stdins["ws-1"][order[2]]
	if !strings.Contains(provision, "jq-1.8.3") || strings.Contains(provision, "1.8.2") || !strings.Contains(plugins, "jq-1.8.3") || strings.Contains(plugins, "1.8.2") {
		t.Errorf("the resume provisioned with %q and staged %q; want only the new pin", provision, plugins)
	}
	if h.machine.ready["ws-1"] != drifted.Stamp || h.machine.ran("ws-1", checkout) != 1 {
		t.Errorf("the resume left stamp %q, or checked out again", h.machine.ready["ws-1"])
	}
}

func TestResumeRefusesWhenTheImageDeclarationChanged(t *testing.T) {
	h := newImagedHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if h.machine.ran("ws-1", provisions) != 0 || h.machine.ran("ws-1", installs) != 1 {
		t.Fatalf("an image host ran %q", h.machine.scripts["ws-1"])
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.Replace(imaged, "user: agent", "user: worker", 1))
	before := len(h.machine.scripts["ws-1"])
	_, err := drifted.Resume(ctx, "ws-1")
	if err == nil || !strings.Contains(err.Error(), "destroy ws-1 and create it again") || !strings.Contains(err.Error(), "agent-host") {
		t.Fatalf("Resume = %v", err)
	}
	if after := strings.Join(h.machine.scripts["ws-1"][before:], "\n"); !strings.Contains(after, readies) || strings.Contains(after, installs) || strings.Contains(after, provisions) {
		t.Errorf("the refused resume ran %q", after)
	}
	if h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the refused resume moved the stamp to %q", h.machine.ready["ws-1"])
	}
}

func TestResumeInstallsPluginsWhenOnlyTheToolsDriftedOnAnImageHost(t *testing.T) {
	h := newImagedHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(imaged + "prepare: [\"echo drifted\"]\n")
	before := len(h.machine.scripts["ws-1"])
	if _, err := drifted.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	order := h.machine.order("ws-1", before, readies, installs, prepares, configures)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("the resume ran scripts out of order: %v", order)
		}
	}
	if strings.Contains(strings.Join(h.machine.scripts["ws-1"][before:], "\n"), provisions) || h.machine.ready["ws-1"] != drifted.Stamp {
		t.Errorf("the resume provisioned an image host in place, or left stamp %q", h.machine.ready["ws-1"])
	}
}

func TestDestroyKeepsAnUnlabelledMachineAndItsAccounting(t *testing.T) {
	for name, created := range map[string]func() time.Time{
		"made an hour earlier":    func() time.Time { return time.Now().Add(-time.Hour) },
		"made during the attempt": time.Now,
		"with no creation time":   func() time.Time { return time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 0, false)
			ctx := context.Background()
			h.fake.Now = created
			if _, err := h.fake.Create(ctx, providers.Spec{Name: "ws-1"}); err != nil {
				t.Fatal(err)
			}
			h.fake.Now = nil
			h.session.Provider = &createFailed{Provider: h.provider}
			if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "run destroy ws-1") {
				t.Fatalf("err = %v", err)
			}
			if record, found := h.record("ws-1"); !found || !record.Unverified || !h.ledger().Running("ws-1") {
				t.Fatalf("an ambiguous create dropped its accounting or left the record verified: %+v, %v", record, found)
			}
			h.session.Provider = h.provider
			if _, err := h.session.Resume(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "never confirmed") {
				t.Fatalf("Resume of an unverified record = %v", err)
			}
			if err := h.session.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "without a label proving this ledger made it") {
				t.Fatalf("Destroy = %v", err)
			}
			if _, err := h.fake.Get(ctx, "ws-1"); err != nil {
				t.Error("destroy removed a machine of unknown ownership")
			}
			if strings.Contains(h.calls(), "destroy") {
				t.Errorf("destroy called the provider: %s", h.calls())
			}
			if record, found := h.record("ws-1"); !found || !record.Unverified || !h.ledger().Running("ws-1") {
				t.Errorf("destroy released a name of unknown ownership: %+v, %v", record, found)
			}
		})
	}
}

type createFailed struct {
	providers.Provider
}

func (c *createFailed) Create(context.Context, providers.Spec) (providers.Machine, error) {
	return providers.Machine{}, errors.New("the provider api timed out before answering")
}

func TestDrainKeepsAnUnlabelledSpareAndStillRemovesTheRest(t *testing.T) {
	h := newHarness(t, 2, false)
	ctx := context.Background()
	stripping := &labelsDroppedOnce{Provider: h.provider}
	h.session.Provider = stripping
	if err := h.session.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	h.session.Provider = h.provider
	unlabelled := stripping.name
	if len(h.spares()) != 2 || unlabelled == "" {
		t.Fatalf("spares = %v, unlabelled %q", h.spares(), unlabelled)
	}
	err := h.session.Drain(ctx, true)
	if err == nil || !strings.Contains(err.Error(), unlabelled) || !strings.Contains(err.Error(), "without a label proving this ledger made it") {
		t.Fatalf("Drain = %v", err)
	}
	if _, err := h.fake.Get(ctx, unlabelled); err != nil {
		t.Error("drain removed a spare of unknown ownership")
	}
	left := h.spares()
	if len(left) != 1 || left[unlabelled] == nil || h.ledger().Resources[unlabelled].Destroyed != nil {
		t.Errorf("drain left %v; want only the unlabelled spare, still accounted", left)
	}
	for id := range h.spares() {
		if id != unlabelled {
			t.Errorf("drain kept the labelled spare %s", id)
		}
	}
	if machines, _ := h.fake.List(ctx, map[string]string{LabelSpare: h.session.Pool.Fingerprint}); len(machines) != 0 {
		t.Errorf("drain left labelled spares %v", machines)
	}
}

type labelsDroppedOnce struct {
	providers.Provider
	name string
}

func (l *labelsDroppedOnce) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if l.name != "" {
		return l.Provider.Create(ctx, spec)
	}
	l.name = spec.Name
	return l.Provider.Create(ctx, providers.Spec{Name: spec.Name})
}

func TestDestroyReportsAMachineThatSurvivesItsOwnDestroy(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	h.session.Provider = &destroyIgnored{Provider: h.provider}
	if err := h.session.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "still at the provider") {
		t.Fatalf("Destroy = %v", err)
	}
	if _, found := h.record("ws-1"); !found || !h.ledger().Running("ws-1") {
		t.Error("a destroy that could not prove absence released the name")
	}
}

type destroyIgnored struct {
	providers.Provider
}

func (d *destroyIgnored) Destroy(context.Context, string) error {
	return nil
}

func TestAPartialCreateKeepsItsRecordUntilDestroyVerifiesTheMachine(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	h.session.Provider = &createdThenFailed{Provider: h.provider}
	_, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "configure-ssh") || !strings.Contains(err.Error(), "run destroy ws-1") {
		t.Fatalf("err = %v", err)
	}
	if _, found := h.record("ws-1"); !found {
		t.Fatal("the record of a machine that may exist was forgotten")
	}
	if resource := h.ledger().Resources["ws-1"]; resource == nil || resource.Destroyed != nil {
		t.Fatalf("the ledger retired a machine that may be billing: %+v", resource)
	}
	if _, err := h.fake.Get(ctx, "ws-1"); err != nil {
		t.Fatal("the created machine was destroyed on an unverified failure")
	}
	if len(h.machine.scripts["ws-1"]) != 0 {
		t.Errorf("the failed create ran %q on a machine it could not confirm", h.machine.scripts["ws-1"])
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.fake.Get(ctx, "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Error("destroy left the machine")
	}
	if resource := h.ledger().Resources["ws-1"]; resource.Destroyed == nil {
		t.Error("destroy did not retire the ledger entry")
	}
	if _, found := h.record("ws-1"); found {
		t.Error("destroy kept the record")
	}
}

type createdThenFailed struct {
	providers.Provider
}

func (c *createdThenFailed) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if _, err := c.Provider.Create(ctx, spec); err != nil {
		return providers.Machine{}, err
	}
	return providers.Machine{}, errors.New("configure-ssh: the provider api timed out")
}

func TestPrepareKeepsAReservationWhoseCreateOutcomeIsUnknownUntilDrainSettlesIt(t *testing.T) {
	h := newHarness(t, 1, false)
	ctx := context.Background()
	h.session.Provider = &createdThenFailed{Provider: h.provider}
	if err := h.session.Prepare(ctx); err == nil || !strings.Contains(err.Error(), "reservation is kept") {
		t.Fatalf("err = %v", err)
	}
	spares := h.spares()
	if len(spares) != 1 {
		t.Fatalf("spares = %v", spares)
	}
	var name string
	for id, spare := range spares {
		name = id
		if spare.State != budget.Preparing {
			t.Errorf("spare = %+v", spare)
		}
	}
	if _, err := h.fake.Get(ctx, name); err != nil {
		t.Fatal("the machine that may exist was destroyed on an unverified failure")
	}
	if err := h.session.Pool.Ledger.UpdateSpares(func(_ *budget.Ledger, spares budget.Spares) error {
		spares[name].Preparer = 1 << 30
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.session.Provider = h.provider
	if err := h.session.Drain(ctx, false); err != nil {
		t.Fatalf("Drain = %v", err)
	}
	if _, err := h.fake.Get(ctx, name); !errors.Is(err, providers.ErrNotFound) {
		t.Error("drain left the machine")
	}
	if len(h.spares()) != 0 || h.ledger().Resources[name].Destroyed == nil {
		t.Error("drain did not retire the reservation")
	}
}

func TestConnectWritesTheSSHFragmentOrcaResolvesByTheWorkspaceName(t *testing.T) {
	h := newHarness(t, 0, false)
	ctx := context.Background()
	knownHosts := filepath.Join(t.TempDir(), "ws-1.known_hosts")
	identityFile := filepath.Join(t.TempDir(), "ws-1")
	h.session.Provider = &pinnedSSH{Provider: h.provider, knownHosts: knownHosts, identity: identityFile}
	result, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	fragment := h.session.State.SSH("ws-1")
	if result.SSH.Config != fragment || !strings.HasPrefix(fragment, filepath.Join(string(h.session.State), "ssh")) || !strings.HasSuffix(fragment, "ws-1.ssh") {
		t.Errorf("result.SSH.Config = %q, fragment %q", result.SSH.Config, fragment)
	}
	raw, err := os.ReadFile(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "Host ws-1\n") {
		t.Errorf("fragment = %q", raw)
	}
	resolved, err := exec.Command("ssh", "-G", "-F", fragment, "ws-1").Output()
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}
	for _, want := range []string{"hostname ws-1.internal", "hostkeyalias ws-1.sprite.cc-remote", "userknownhostsfile " + knownHosts, "identityfile " + identityFile, "stricthostkeychecking true", "proxycommand cc-remote proxy -- ws-1", "port 2222", "user sprite"} {
		if !strings.Contains(strings.ToLower(string(resolved)), strings.ToLower(want)) {
			t.Errorf("ssh -G did not resolve %q from the fragment:\n%s", want, resolved)
		}
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fragment); !errors.Is(err, os.ErrNotExist) {
		t.Error("destroy left the ssh fragment behind")
	}
}

type pinnedSSH struct {
	providers.Provider
	knownHosts string
	identity   string
}

func (p *pinnedSSH) SSHTarget(_ context.Context, id string) (providers.Target, error) {
	return providers.Target{
		Host:          id + ".internal",
		Port:          2222,
		User:          "sprite",
		IdentityFile:  p.identity,
		ProxyCommand:  "cc-remote proxy -- " + id,
		HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyPinned, Alias: id + ".sprite.cc-remote", KnownHostsFile: p.knownHosts},
	}, nil
}
