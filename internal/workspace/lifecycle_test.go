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
	suffix   = "example.ts.net"
	tag      = "tag:cc-remote"
	token    = "ghp_" + "testtokenthatmustnotreachascript00000000"
	enrolls  = ` up --auth-key="file:`
	freshens = `test ! -e "/var/lib/tailscale/tailscaled.state"`
	logsOut  = "tailscale --socket=/run/tailscale/tailscaled.sock logout"
	renews   = `rm -rf "$HOME"/.claude.json`
	checkout = "git clone --quiet"
	prepares = "cd /home/fake/app\n"
	warms    = "warm-head"
	cleans   = "this spare is not clean"
	noState  = `{"BackendState":"NoState"}`
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
	script := cmd[2]
	m.mu.Lock()
	if m.scripts == nil {
		m.scripts, m.stdins = map[string][]string{}, map[string][]string{}
	}
	m.scripts[id] = append(m.scripts[id], script)
	m.stdins[id] = append(m.stdins[id], strings.TrimSpace(string(stdin)))
	failAll := m.failAll
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
	t        *testing.T
	session  *Session
	fake     *providertest.Fake
	provider *flaky
	machine  *scripted
	api      *fakeTailnet
}

func newHarness(t *testing.T, spares int, withTailnet bool) *harness {
	t.Helper()
	machine := &scripted{status: noState, minted: "nNEW"}
	fake := &providertest.Fake{Rates: providers.Rate{HourlyUSD: 1}, Handle: machine.Handle}
	provider := &flaky{Provider: fake}
	bootstrap := filepath.Join(t.TempDir(), "bootstrap.sh")
	if err := os.WriteFile(bootstrap, []byte("#!/bin/sh\necho bootstrapped\n"), 0o700); err != nil {
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
      fake: {}
spares:
  fake: { lean: %d }
bootstrap: %s
forwards:
  - { label: web, env: WEB_PORT }
budget:
  cap_usd: 100
  reserve_usd: 10
  trial_hours: 1
`, t.TempDir(), spares, bootstrap)
	if withTailnet {
		text += "tailnet:\n  tag: " + tag + "\n"
	}
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	session, err := Open(cfg, provider, "fake", "lean", Platform{Daemon: tailnet.Daemon{Mode: tailnet.Kernel, Supervisor: tailnet.SpriteEnv}, HostKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	session.Log = slog.New(slog.DiscardHandler)
	session.Stderr = io.Discard
	session.Token = func(context.Context) (string, error) { return token, nil }
	api := &fakeTailnet{devices: []tailnet.Device{owned("nNEW")}}
	if withTailnet {
		client := api.serve(t)
		session.Enroller.Connect = func(context.Context) (*tailnet.Client, error) { return client, nil }
		session.Enroller.Log = session.Log
	}
	return &harness{t: t, session: session, fake: fake, provider: provider, machine: machine, api: api}
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
	result, err := h.session.Create(context.Background(), "ws-1", "feature")
	if err != nil {
		t.Fatal(err)
	}
	order := h.machine.order("ws-1", 0, checkout, prepares, "bootstrap.sh", freshens, enrolls)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("scripts ran out of order: %v", order)
		}
	}
	if h.machine.ran("ws-1", renews) != 0 || h.machine.ran("ws-1", warms) != 0 {
		t.Error("a fresh create renewed identity or warmed")
	}
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
	if result.Tailnet == nil || result.Tailnet.NodeID != "nNEW" || result.SSH.Host != "ws-1" || len(result.Forwards) != 1 || result.Ref != "feature" {
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
	result, err := h.session.Create(context.Background(), "ws-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	order := h.machine.order(spare, before, renews, checkout, prepares, warms, freshens, enrolls)
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] <= order[i-1] {
			t.Fatalf("the claim ran scripts out of order: %v (prepare ran %d)", order, before)
		}
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
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err != nil {
		t.Fatal(err)
	}
	identities, minted := h.machine.ran(spare, renews), h.api.mintedKeys()
	calls := len(h.fake.Calls())
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err != nil {
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
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err != nil {
		t.Fatal(err)
	}
	h.machine.failAll = true
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err == nil {
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
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err == nil || !strings.Contains(err.Error(), "never finished") {
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
			if _, err := h.session.Create(context.Background(), "ws-1", "main"); err == nil || !strings.Contains(err.Error(), "already exists") {
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
	_, err := h.session.Create(context.Background(), "ws-1", "main")
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
	_, err := h.session.Create(context.Background(), "ws-1", "main")
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
			h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Ref: "main", Machine: "ws-1"})
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
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Ref: "main", Machine: "ws-1"})
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
	if _, err := h.session.Create(ctx, "ws-1", "main"); err != nil {
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
	if _, err := h.session.Create(ctx, "ws-1", "main"); err != nil {
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
	if _, err := h.session.Create(ctx, "ws-1", "main"); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if h.ledger().Running("ws-1") || !strings.Contains(h.calls(), "suspend ws-1") {
		t.Error("suspend left the workspace running")
	}
	checkouts := h.machine.ran("ws-1", checkout)
	result, err := h.session.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if !h.ledger().Running("ws-1") || h.machine.ran("ws-1", checkout) != checkouts || h.machine.ran("ws-1", prepares) < 2 {
		t.Error("resume did not restart the ledger, or re-cloned, or skipped the prepare steps")
	}
	bootstraps := h.machine.order("ws-1", 0, "bootstrap.sh")
	if last := h.machine.stdins["ws-1"][len(h.machine.stdins["ws-1"])-1]; bootstraps[0] < 0 || last != "" {
		t.Errorf("resume sent %q to the bootstrap instead of no token", last)
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
	if _, err := h.session.Create(context.Background(), "ws-1", "main"); err != nil {
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
