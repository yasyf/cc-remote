package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const (
	suffix         = "example.ts.net"
	tag            = "tag:cc-remote"
	token          = "ghp_" + "testtokenthatmustnotreachascript00000000"
	enrolls        = ` up --auth-key="file:`
	freshens       = `! sudo -n test -e "/var/lib/tailscale/tailscaled.state"`
	logsOut        = "tailscale --socket=/run/tailscale/tailscaled.sock logout"
	renews         = `rm -rf "$HOME"/.claude.json`
	checkout       = "clone --quiet"
	prepares       = "cd /home/fake/app\n"
	noState        = `{"BackendState":"NoState"}`
	provisions     = "sudo bash -s"
	prereqsPhase   = "sudo bash -s prerequisites"
	packagesPhase  = "sudo bash -s packages"
	toolsPhase     = "sudo bash -s tools"
	payloadPhase   = "sudo --preserve-env=PATH bash -s payload "
	loaderPhase    = "sudo bash -s loader"
	mergesPlugins  = "stage-plugins "
	stagesPayload  = "stage-payload "
	fetchesPayload = "fetch-payload "
	stagesDebs     = "stage-packages "
	fetchesDebs    = "fetch-packages "
	payloadBytes   = "hsqs squashfs payload bytes"
	packagesBytes  = "resident packages archive bytes"
	packagesSHA    = "4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	urlSentinel    = "X-Amz-Signature=5e11ab1e5e11ab1e5e11ab1e5e11ab1e"
	payloadURL     = "https://bucket.s3.us-west-2.amazonaws.com/cache-seeds/sources/" + sha + "/tools.sqfs?X-Amz-Algorithm=AWS4-HMAC-SHA256&" + urlSentinel
	packagesURL    = "https://bucket.s3.us-west-2.amazonaws.com/cache-seeds/sources/" + packagesSHA + "/debs.tar?X-Amz-Algorithm=AWS4-HMAC-SHA256&" + urlSentinel
	stages         = "plugins.sh.tmp"
	installs       = "plugins.sh install"
	publishes      = "plugins.sh publish "
	readies        = "plugins.sh ready "
	configures     = "plugins.sh configure"
	inventory      = systemHead + staticTailnet + configureEnv
	systemHead     = "version: 1\nsystem:\n  - { name: jq, version: 1.8.2, url: https://example.com/jq-1.8.2, sha256: " + sha + ", format: binary }\n"
	staticTailnet  = "  - { name: tailscale, version: 1.90.0, url: https://example.com/tailscale.tgz, sha256: " + sha + ", format: tar.gz, bins: { tailscale: tailscale/tailscale, tailscaled: tailscale/tailscaled } }\n"
	debTailnet     = "  - { name: tailscale, version: 1.90.0, url: https://example.com/tailscale.deb, sha256: " + sha + ", format: deb, bins: { tailscale: /usr/bin/tailscale, tailscaled: /usr/sbin/tailscaled } }\n"
	cliTailnet     = "  - { name: tailscale, version: 1.90.0, url: https://example.com/tailscale.tgz, sha256: " + sha + ", format: tar.gz, bins: { tailscale: tailscale/tailscale } }\n"
	configureEnv   = "configure:\n  env: [WEB_PORT]\n"
	closureApt     = "apt:\n  install: [openssh-server, libnss3]\n  payload:\n    resident: [bubblewrap]\n    closure: [libnss3]\n    bins: [certutil]\n"
	imaged         = "version: 1\nimage:\n  name: agent-host\n  base: ubuntu:24.04@sha256:" + sha + "\n  user: agent\n  workspaceDir: /workspaces\nconfigure:\n  env: [WEB_PORT]\n"
	sha            = "008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3"
	commit         = "008173c23f95b170204355c12626cb5a965d779a"
	private        = "version: 1\nsystem:\n  - { name: claude, version: 2.0.0, url: https://example.com/claude, sha256: " + sha + ", format: binary }\nclaude:\n  marketplaces:\n    - { name: market, github: owner/market, ref: " + commit + ", private: true }\nconfigure:\n  env: [WEB_PORT]\n"
)

func running(nodeID string) string {
	return `{"BackendState":"Running","Self":{"ID":"` + nodeID + `","DNSName":"ws-1.` + suffix + `.","TailscaleIPs":["100.64.0.9"]}}`
}

type scripted struct {
	mu            sync.Mutex
	status        string
	minted        string
	fail          error
	failAll       bool
	joined        bool
	tailscaleFrom string
	tailscale     bool
	onEnroll      func()
	onStatus      func()
	scripts       map[string][]string
	stdins        map[string][]string
	ready         map[string]string
	holds         map[string]*hold
	baked         string
}

type hold struct {
	fragment string
	machine  string
	entered  chan struct{}
	release  chan providers.Result
}

func (m *scripted) hold(t *testing.T, fragment string) *hold {
	t.Helper()
	return m.holdOn(t, "", fragment)
}

func (m *scripted) holdOn(t *testing.T, machine, fragment string) *hold {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holds == nil {
		m.holds = map[string]*hold{}
	}
	h := &hold{fragment: fragment, machine: machine, entered: make(chan struct{}), release: make(chan providers.Result, 1)}
	m.holds[fragment] = h
	t.Cleanup(func() { close(h.release) })
	return h
}

func (h *hold) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%q never ran", h.fragment)
	}
}

func (h *hold) stillParked(t *testing.T, stage string) {
	t.Helper()
	select {
	case <-h.entered:
		t.Fatalf("%q ran %s", h.fragment, stage)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *hold) ran() bool {
	select {
	case <-h.entered:
		return true
	default:
		return false
	}
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
	result := m.handle(id, script, cmd[len(cmd)-1], stdin)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tailscaleFrom != "" && result.ExitCode == 0 && strings.Contains(script, m.tailscaleFrom) {
		m.tailscale = true
	}
	return result
}

func (m *scripted) handle(id, script, stamp string, stdin []byte) providers.Result {
	m.mu.Lock()
	if m.scripts == nil {
		m.scripts, m.stdins, m.ready = map[string][]string{}, map[string][]string{}, map[string]string{}
	}
	m.scripts[id] = append(m.scripts[id], script)
	m.stdins[id] = append(m.stdins[id], strings.TrimSpace(string(stdin)))
	failAll, joined, baked := m.failAll, m.joined, m.baked
	missing := m.tailscaleFrom != "" && !m.tailscale && strings.Contains(script, "tailscale")
	var held *hold
	for fragment, h := range m.holds {
		if strings.Contains(script, fragment) && (h.machine == "" || h.machine == id) {
			held = h
		}
	}
	switch {
	case strings.Contains(script, publishes):
		m.ready[id] = stamp
	case strings.Contains(script, readies) && m.ready[id] != stamp:
		m.mu.Unlock()
		return providers.Result{Stderr: []byte("plugins: this host was not prepared from stamp " + stamp), ExitCode: 1}
	}
	m.mu.Unlock()
	if missing {
		if held != nil {
			close(held.entered)
		}
		return providers.Result{Stderr: []byte("sh: tailscale: command not found"), ExitCode: 127}
	}
	if held != nil {
		close(held.entered)
		return <-held.release
	}
	if failAll {
		return providers.Result{Stderr: []byte("the machine went away"), ExitCode: 1}
	}
	if joined && strings.Contains(script, freshens) {
		return providers.Result{Stderr: []byte("cc-remote: this machine already carries /var/lib/tailscale/tailscaled.state, so its image joined a tailnet before this workspace was created; rebuild it unenrolled"), ExitCode: 1}
	}
	switch {
	case strings.Contains(script, images.ManifestPath):
		return providers.Result{Stdout: []byte(baked)}
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
	t         *testing.T
	session   *Session
	fake      *providertest.Fake
	provider  *flaky
	machine   *scripted
	api       *fakeTailnet
	cfg       *config.Config
	inventory string
}

func newHarness(t *testing.T, withTailnet bool) *harness {
	t.Helper()
	return build(t, withTailnet, inventory, "{}", providers.Traits{})
}

func newImagedHarness(t *testing.T) *harness {
	t.Helper()
	h := build(t, false, imaged, "{ image: agent-host }", providers.Traits{Platform: images.DefaultPlatform})
	h.machine.baked = string(h.session.baked)
	return h
}

func newDirectPayloadHarness(t *testing.T, script string) (*harness, string) {
	t.Helper()
	_, archive := closureFiles(t)
	h := build(t, false, inventory+closureApt, fmt.Sprintf("{ payload: { url_command: [./payload-url, --expires-in, \"900\"], sha256: %s, size: %d, packages: { path: %q, sha256: %s } } }", sha, len(payloadBytes), archive, packagesSHA), providers.Traits{Supervisor: providers.SupervisorSpriteEnv})
	dir := filepath.Dir(h.cfg.Path)
	recorded := filepath.Join(dir, "recorded")
	command := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + recorded + ".args\nenv > " + recorded + ".env\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "payload-url"), []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	return h, recorded
}

func closureFiles(t *testing.T) (payload, archive string) {
	t.Helper()
	dir := t.TempDir()
	payload, archive = filepath.Join(dir, "agent.sqfs"), filepath.Join(dir, "agent-debs.tar")
	for file, content := range map[string]string{payload: payloadBytes, archive: packagesBytes} {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return payload, archive
}

func newPayloadHarness(t *testing.T) *harness {
	t.Helper()
	path, archive := closureFiles(t)
	return build(t, false, inventory+closureApt, fmt.Sprintf("{ payload: { path: %q, sha256: %s, packages: { path: %q, sha256: %s } } }", path, sha, archive, packagesSHA), providers.Traits{Supervisor: providers.SupervisorSpriteEnv})
}

func newDirectPackagesHarness(t *testing.T, script string) (*harness, string) {
	t.Helper()
	path, _ := closureFiles(t)
	h := build(t, false, inventory+closureApt, fmt.Sprintf("{ payload: { path: %q, sha256: %s, packages: { url_command: [./packages-url], sha256: %s, size: %d } } }", path, sha, packagesSHA, len(packagesBytes)), providers.Traits{Supervisor: providers.SupervisorSpriteEnv})
	dir := filepath.Dir(h.cfg.Path)
	recorded := filepath.Join(dir, "recorded-packages")
	command := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + recorded + ".args\nenv > " + recorded + ".env\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "packages-url"), []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	return h, recorded
}

func build(t *testing.T, withTailnet bool, tools, machineSpec string, facts providers.Traits) *harness {
	t.Helper()
	machine := &scripted{status: noState, minted: "nNEW"}
	fake := &providertest.Fake{Facts: facts, Handle: machine.Handle}
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
    machine:
      fake: %s
inventory: %s
forwards:
  - { label: web, env: WEB_PORT }
`, t.TempDir(), machineSpec, inventoryPath)
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
	session, err := Open(h.cfg, h.provider, "fake", "lean", Platform{Daemon: tailnet.Daemon{Mode: tailnet.Kernel, Supervisor: tailnet.SpriteEnv}})
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

func (h *harness) reimage(image string) *Session {
	h.t.Helper()
	machine := h.cfg.Profiles["lean"].Machine["fake"]
	machine.Image = image
	h.cfg.Profiles["lean"].Machine["fake"] = machine
	moved := h.open()
	if moved.Stamp != h.session.Stamp {
		h.t.Fatal("the image reference moved the stamp")
	}
	return moved
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

func (h *harness) awaitBound(want tailnet.Binding) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.bound() != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("never bound to %v; bound %v", want, h.bound())
		}
		time.Sleep(5 * time.Millisecond)
	}
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

func (h *harness) calls() string {
	return strings.Join(h.fake.Calls(), " ")
}

func (h *harness) ordered(from int, fragments ...string) []int {
	h.t.Helper()
	order := h.machine.order("ws-1", from, fragments...)
	for i := range order {
		if order[i] < 0 || (i > 0 && order[i] <= order[i-1]) {
			h.t.Fatalf("scripts ran out of order: %v for %q in %q", order, fragments, h.machine.scripts["ws-1"][from:])
		}
	}
	return order
}

func TestAFreshCreateChecksOutPreparesBootstrapsThenEnrolls(t *testing.T) {
	h := newHarness(t, true)
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	h.ordered(0, prereqsPhase, packagesPhase, publishes)
	h.ordered(0, prereqsPhase, checkout)
	h.ordered(0, prereqsPhase, toolsPhase)
	toolsLane := h.ordered(0, toolsPhase, stages, installs, configures, publishes)
	h.ordered(0, installs, freshens, enrolls, publishes)
	h.ordered(0, packagesPhase, configures)
	source := h.ordered(0, checkout, prepares, publishes)
	if h.machine.ran("ws-1", renews) != 0 || h.machine.ran("ws-1", readies) != 0 || h.machine.ran("ws-1", payloadPhase) != 0 || h.machine.ran("ws-1", mergesPlugins) != 0 || h.machine.ran("ws-1", publishes) != 1 || h.machine.ran("ws-1", prereqsPhase) != 1 {
		t.Errorf("a fresh create renewed identity, checked a ready stamp, mounted a payload, merged its plugins, or ran its prerequisites or publish other than once: %q", h.machine.scripts["ws-1"])
	}
	if got := h.machine.stdins["ws-1"][toolsLane[2]]; got != "" {
		t.Errorf("the tool install read %q on stdin although no private marketplace needs a token", got)
	}
	if got := h.machine.scripts["ws-1"][toolsLane[2]]; !strings.HasSuffix(got, installs) {
		t.Errorf("the install named a payload on a machine without one: %q", got)
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, token) || strings.Contains(script, "tskey-auth") {
			t.Errorf("script %d carries a secret", i)
		}
	}
	if got := h.machine.stdins["ws-1"][source[0]]; got != token {
		t.Errorf("checkout read %q on stdin", got)
	}
	if !strings.Contains(h.machine.scripts["ws-1"][source[0]], "--depth 1") || !strings.Contains(h.machine.scripts["ws-1"][source[0]], "ref=feature") {
		t.Error("the lean checkout is not a shallow checkout of the requested ref")
	}
	if result.Machine != "ws-1" || !strings.Contains(h.calls(), "create ws-1") || result.Tailnet == nil || result.Tailnet.NodeID != "nNEW" || result.SSH.Host != "ws-1" || len(result.Forwards) != 1 || result.Source.Ref != "feature" {
		t.Errorf("result = %+v", result)
	}
	if record, found := h.record("ws-1"); !found || record.Tailnet == nil || record.Tailnet.NodeID != "nNEW" {
		t.Errorf("record = %+v, %v", record, found)
	}
	if h.bound() != (tailnet.Binding{NodeID: "nNEW"}) {
		t.Errorf("bound %v", h.bound())
	}
	machine, _ := h.fake.Get(context.Background(), "ws-1")
	if machine.Labels[LabelWorkspace] != "ws-1" || machine.Labels[LabelProfile] != "lean" {
		t.Errorf("labels = %v", machine.Labels)
	}
}

func TestARetriedFreshCreateLeavesTheWorkspaceItMade(t *testing.T) {
	h := newHarness(t, false)
	if err := h.session.save(&Record{Name: "ws-1", Provider: h.session.Kind, Profile: h.session.Profile, Machine: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	h.machine.failAll = true
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the retry of a fresh create returned %v; want a refusal before any machine call", err)
	}
	if len(h.fake.Calls()) != 0 {
		t.Errorf("the refused retry ran %v on the machine", h.fake.Calls())
	}
}

func TestAFailedCreateLeavesTheTailnetThenDiscardsWhatItMade(t *testing.T) {
	h := newHarness(t, true)
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
	if _, found := h.record("ws-1"); found {
		t.Error("the failed create left a record")
	}
}

func TestAFailedCreateDestroysTheMachineEvenWhenItsNodeCannotBeRevoked(t *testing.T) {
	h := newHarness(t, true)
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
			h := newHarness(t, true)
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
	h := newHarness(t, true)
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
	h := newHarness(t, false)
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

func TestDestroyLeavesTheTailnetBeforeTheMachine(t *testing.T) {
	h := newHarness(t, true)
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
	if _, found := h.record("ws-1"); found {
		t.Error("the record survived destroy")
	}
	if err := h.session.Destroy(ctx, "ws-1"); err == nil {
		t.Error("a second destroy found a workspace")
	}
}

func TestSuspendAndResumeRetainTheWorkspace(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.calls(), "suspend ws-1") {
		t.Error("suspend left the workspace running")
	}
	checkouts := h.machine.ran("ws-1", checkout)
	before := len(h.machine.scripts["ws-1"])
	result, err := h.session.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if h.machine.ran("ws-1", checkout) != checkouts || h.machine.ran("ws-1", prepares) < 2 {
		t.Error("resume re-cloned or skipped the prepare steps")
	}
	if resumed := strings.Join(h.machine.scripts["ws-1"][before:], "\n"); !strings.Contains(resumed, readies) || strings.Contains(resumed, installs) || !strings.Contains(resumed, configures) {
		t.Errorf("resume did not check the ready stamp, or reinstalled, or skipped configure: %q", resumed)
	}
	if result.Forwards[0].Port == 0 {
		t.Error("resume lost the forwarded port")
	}
}

func TestResumeRefusesAnUnknownOrForeignWorkspace(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.session.Resume(context.Background(), "ws-1"); err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Errorf("err = %v", err)
	}
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "full", Machine: "ws-1"})
	if _, err := h.session.Resume(context.Background(), "ws-1"); err == nil || !strings.Contains(err.Error(), "fake/full") {
		t.Errorf("err = %v", err)
	}
}

func TestStatusReportsWorkspaces(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	status, err := h.session.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Workspaces) != 1 || status.Workspaces[0].Name != "ws-1" || status.Workspaces[0].Machine != "ws-1" {
		t.Errorf("workspaces = %+v", status.Workspaces)
	}
}

func TestAFreshCreateNeverDestroysAMachineItDidNotMake(t *testing.T) {
	h := newHarness(t, true)
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
}

func TestDestroyHoldsTheResourceUntilTheMachineIsGone(t *testing.T) {
	h := newHarness(t, false)
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

func TestAFailedCreateKeepsItsRecordWhenTheMachineCannotBeRemoved(t *testing.T) {
	h := newHarness(t, false)
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

func TestCreateRefusesANameStillBoundToATailnetNodeAndDestroyRevokesIt(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	h.api.devices = append(h.api.devices, owned("nOLD"))
	h.bind(tailnet.Binding{NodeID: "nOLD"})
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "still bound") {
		t.Fatalf("err = %v", err)
	}
	if len(h.fake.Calls()) != 0 || len(h.api.deletedNodes()) != 0 {
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

func TestTheInstallReadsTheTokenOnlyForPrivateMarketplaces(t *testing.T) {
	h := build(t, false, private, "{}", providers.Traits{})
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	order := h.ordered(0, installs)
	if h.machine.stdins["ws-1"][order[0]] != token {
		t.Errorf("the install read %q on stdin; want the token for the private marketplace", h.machine.stdins["ws-1"][order[0]])
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, token) {
			t.Errorf("script %d carries the token", i)
		}
	}
}

func TestAPayloadIsStagedAndMountedBeforeTheToolsAndHandedToTheInstall(t *testing.T) {
	h := newPayloadHarness(t)
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	order := h.ordered(0, stagesPayload, payloadPhase, toolsPhase, mergesPlugins, installs, configures, publishes)
	h.ordered(0, prereqsPhase, stagesPayload)
	h.ordered(0, packagesPhase, publishes)
	h.ordered(0, checkout, prepares, publishes)
	if stage := h.machine.scripts["ws-1"][order[0]]; !strings.HasPrefix(stage, "sudo bash -c ") || !strings.HasSuffix(stage, " "+stagesPayload+sha) {
		t.Errorf("the payload was staged by %q", stage)
	}
	if staged := h.machine.stdins["ws-1"][order[0]]; staged != payloadBytes {
		t.Errorf("the staging read %q on stdin, want the payload file", staged)
	}
	if mount := h.machine.scripts["ws-1"][order[1]]; mount != payloadPhase+sha+" "+h.session.Scripts.Fingerprint()+" "+packagesSHA {
		t.Errorf("the payload phase ran as %q", mount)
	}
	if merge := h.machine.scripts["ws-1"][order[3]]; !strings.HasPrefix(merge, "sh -c ") || !strings.Contains(merge, stages) || !strings.HasSuffix(merge, " "+mergesPlugins+images.PayloadRoot+"/"+sha) {
		t.Errorf("the plugin flags were merged by %q; want the user's shell staging plugins.sh with the payload directory as its argument", merge)
	}
	if staged := h.machine.stdins["ws-1"][order[3]]; staged != strings.TrimSpace(string(h.session.Scripts.Plugins)) {
		t.Errorf("the merge read %.40q on stdin, want plugins.sh", staged)
	}
	if h.machine.ran("ws-1", stages) != 1 {
		t.Errorf("plugins.sh was staged %d times, want once together with the merge", h.machine.ran("ws-1", stages))
	}
	if install := h.machine.scripts["ws-1"][order[4]]; !strings.HasSuffix(install, installs+" "+images.PayloadRoot+"/"+sha) {
		t.Errorf("the install ran as %q; want the payload directory as its argument", install)
	}
	if h.machine.ran("ws-1", stagesPayload) != 1 || h.machine.ran("ws-1", payloadPhase) != 1 || h.machine.ran("ws-1", mergesPlugins) != 1 || h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the payload was staged %d times, mounted %d times, and merged %d times, and the stamp is %q", h.machine.ran("ws-1", stagesPayload), h.machine.ran("ws-1", payloadPhase), h.machine.ran("ws-1", mergesPlugins), h.machine.ready["ws-1"])
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, token) || strings.Contains(script, payloadBytes) {
			t.Errorf("script %d carries a secret or the payload itself", i)
		}
	}
}

func TestADirectPayloadIsFetchedOverStdinAndMountedWithoutTheURLLeaking(t *testing.T) {
	h, recorded := newDirectPayloadHarness(t, "printf '%s\\n' '"+payloadURL+"'\nprintf 'presigned %s\\n' '"+payloadURL+"' >&2")
	var logs, stderr bytes.Buffer
	h.session.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	h.session.Stderr = &stderr
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	order := h.ordered(0, prereqsPhase, fetchesPayload, payloadPhase, toolsPhase, mergesPlugins, installs, configures, publishes)
	h.ordered(0, packagesPhase, publishes)
	h.ordered(0, checkout, prepares, publishes)
	size := strconv.Itoa(len(payloadBytes))
	if fetch := h.machine.scripts["ws-1"][order[1]]; !strings.HasPrefix(fetch, "sudo bash -c ") || !strings.HasSuffix(fetch, " "+fetchesPayload+sha+" "+size) {
		t.Errorf("the payload was fetched by %q", fetch)
	}
	if got := h.machine.stdins["ws-1"][order[1]]; got != `url = "`+payloadURL+`"` {
		t.Errorf("the fetch read %q on stdin, want the curl config carrying the URL", got)
	}
	if mount := h.machine.scripts["ws-1"][order[2]]; mount != payloadPhase+sha+" "+h.session.Scripts.Fingerprint()+" "+packagesSHA {
		t.Errorf("the payload phase ran as %q", mount)
	}
	if h.machine.ran("ws-1", stagesPayload) != 0 || h.machine.ran("ws-1", fetchesPayload) != 1 || h.machine.ran("ws-1", payloadPhase) != 1 || h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the payload was staged %d times, fetched %d times, and mounted %d times, and the stamp is %q", h.machine.ran("ws-1", stagesPayload), h.machine.ran("ws-1", fetchesPayload), h.machine.ran("ws-1", payloadPhase), h.machine.ready["ws-1"])
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, urlSentinel) || (i != order[1] && strings.Contains(h.machine.stdins["ws-1"][i], urlSentinel)) {
			t.Errorf("exec %d carries the payload URL outside the fetch's stdin: %q", i, script)
		}
	}
	args, err := os.ReadFile(recorded + ".args")
	if err != nil {
		t.Fatal(err)
	}
	if want := "--expires-in\n900\n" + sha + "\n" + size + "\n"; string(args) != want {
		t.Errorf("the url command ran with %q, want %q", args, want)
	}
	env, err := os.ReadFile(recorded + ".env")
	if err != nil {
		t.Fatal(err)
	}
	marshalled, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(h.session.State.Workspace("ws-1"))
	if err != nil {
		t.Fatal(err)
	}
	ssh, err := os.ReadFile(h.session.State.SSH("ws-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"fetched the payload"`) {
		t.Errorf("the log lacks the fetch milestone:\n%s", logs.String())
	}
	for name, text := range map[string]string{"the url command's environment": string(env), "the log": logs.String(), "stderr": stderr.String(), "the result": string(marshalled), "the workspace record": string(record), "the ssh fragment": string(ssh)} {
		if strings.Contains(text, urlSentinel) {
			t.Errorf("%s carries the payload URL", name)
		}
	}
}

func TestAFailedURLCommandFailsTheCreateBeforeTheMachine(t *testing.T) {
	tests := []struct {
		name, script, want string
	}{
		{"a nonzero exit", "echo 'the sso session expired' >&2\nexit 3", "payload url_command ./payload-url: exit status 3"},
		{"a url beside a nonzero exit", "printf '%s\\n' '" + payloadURL + "'\nexit 1", "payload url_command ./payload-url: exit status 1"},
		{"a url on stderr beside a nonzero exit", "printf 'aws s3 presign %s failed\\n' '" + payloadURL + "' >&2\nexit 1", "payload url_command ./payload-url: exit status 1"},
		{"a url on stderr beside a kill", "printf 'aws s3 presign %s failed\\n' '" + payloadURL + "' >&2\nkill -TERM $$", "payload url_command ./payload-url: signal: terminated"},
		{"a plain http url", "printf 'http://bucket.example/tools.sqfs\\n'", "payload url_command ./payload-url: payload url: the URL must be an absolute https URL with a host and no userinfo"},
		{"two urls", "printf '%s\\n%s\\n' '" + payloadURL + "' '" + payloadURL + "'", "payload url_command ./payload-url printed more than one line"},
		{"no output", ":", "payload url_command ./payload-url: payload url: the command printed nothing"},
		{"a blank line", "printf ' \\n'", "payload url_command ./payload-url: payload url: the URL must be printable ASCII without spaces, quotes or backslashes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newDirectPayloadHarness(t, tt.script)
			var logs, stderr bytes.Buffer
			h.session.Log = slog.New(slog.NewJSONHandler(&logs, nil))
			h.session.Stderr = &stderr
			_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Create = %v, want %q", err, tt.want)
			}
			if stderr.Len() != 0 {
				t.Errorf("the url command's stderr reached the CLI: %q", stderr.String())
			}
			if len(h.fake.Calls()) != 0 {
				t.Errorf("a create without a payload URL reached the provider: %q", h.fake.Calls())
			}
			if _, ok := h.record("ws-1"); ok {
				t.Error("the failed create kept its record")
			}
			for name, text := range map[string]string{"the error": err.Error(), "the log": logs.String(), "stderr": stderr.String()} {
				if strings.Contains(text, urlSentinel) {
					t.Errorf("%s carries the payload URL", name)
				}
			}
		})
	}
}

func TestAnExpiredDirectPayloadAbandonsTheMachine(t *testing.T) {
	h, _ := newDirectPayloadHarness(t, "printf '%s\\n' '"+payloadURL+"'")
	expired := h.machine.hold(t, fetchesPayload)
	expired.release <- providers.Result{Stderr: []byte("cc-remote: the payload download failed (curl exit 22, HTTP 403, tee exit 0, openssl exit 0); a 403 usually means the presigned URL expired"), ExitCode: 1}
	var logs bytes.Buffer
	h.session.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "fetch payload "+sha+": ") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Create = %v", err)
	}
	if !expired.ran() || h.machine.ran("ws-1", payloadPhase) != 0 || h.machine.ran("ws-1", mergesPlugins) != 0 || h.machine.ran("ws-1", installs) != 0 || h.machine.ran("ws-1", publishes) != 0 {
		t.Errorf("an expired download ran %q", h.machine.scripts["ws-1"])
	}
	if !strings.Contains(h.calls(), "destroy ws-1") {
		t.Errorf("the machine survived the failed download: %s", h.calls())
	}
	if _, ok := h.record("ws-1"); ok {
		t.Error("the failed create kept its record")
	}
	for name, text := range map[string]string{"the error": err.Error(), "the log": logs.String()} {
		if strings.Contains(text, urlSentinel) {
			t.Errorf("%s carries the payload URL", name)
		}
	}
}

func TestAResumeAtANewStampFetchesTheDirectPayloadAgain(t *testing.T) {
	h, recorded := newDirectPayloadHarness(t, "printf '%s\\n' '"+payloadURL+"'")
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recorded + ".args"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.NewReplacer("1.8.2", "1.8.3", sha, strings.Repeat("ab", 32)).Replace(inventory + closureApt))
	before := len(h.machine.scripts["ws-1"])
	if _, err := drifted.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recorded + ".args"); err != nil {
		t.Errorf("the resume did not ask the url command again: %v", err)
	}
	h.ordered(before, readies, prereqsPhase, fetchesPayload, payloadPhase, toolsPhase, mergesPlugins, installs, publishes)
	if h.machine.ran("ws-1", stagesPayload) != 0 || h.machine.ran("ws-1", fetchesPayload) != 2 || h.machine.ready["ws-1"] != drifted.Stamp {
		t.Errorf("the payload was staged %d times and fetched %d times, and the stamp is %q, want %q", h.machine.ran("ws-1", stagesPayload), h.machine.ran("ws-1", fetchesPayload), h.machine.ready["ws-1"], drifted.Stamp)
	}
}

func TestAMissingPayloadFileFailsTheCreateBeforeAnythingIsPublished(t *testing.T) {
	h := newPayloadHarness(t)
	if err := os.Remove(h.cfg.Profiles["lean"].Machine["fake"].Payload.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "open payload: ") {
		t.Fatalf("Create = %v", err)
	}
	if h.machine.ran("ws-1", stagesPayload) != 0 || h.machine.ran("ws-1", mergesPlugins) != 0 || h.machine.ran("ws-1", installs) != 0 || h.machine.ran("ws-1", publishes) != 0 {
		t.Errorf("a create without its payload file ran %q", h.machine.scripts["ws-1"])
	}
	if _, ok := h.record("ws-1"); ok {
		t.Error("the failed create kept its record")
	}
}

func TestAPayloadPathMustResolveToARegularFile(t *testing.T) {
	notRegular := func(path string, info os.FileInfo) string {
		return fmt.Sprintf("open payload: %s is not a regular file (mode %v)", path, info.Mode())
	}
	shared := func(path string, info os.FileInfo) string {
		return fmt.Sprintf("open payload: %s is readable by others (mode %v); only its owner may read a payload", path, info.Mode())
	}
	tests := []struct {
		name    string
		replace func(path, staged string) error
		refused func(path string, info os.FileInfo) string
	}{
		{name: "a private payload is accepted", replace: func(path, staged string) error { return os.Rename(staged, path) }},
		{name: "a symlink to a private payload is followed", replace: func(path, staged string) error { return os.Symlink(staged, path) }},
		{name: "a directory is refused", replace: func(path, _ string) error { return os.Mkdir(path, 0o700) }, refused: notRegular},
		{name: "a fifo is refused without waiting for a writer", replace: func(path, _ string) error { return syscall.Mkfifo(path, 0o600) }, refused: notRegular},
		{name: "a payload readable by others is refused", replace: func(path, staged string) error {
			if err := os.Rename(staged, path); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
		}, refused: shared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPayloadHarness(t)
			path := h.cfg.Profiles["lean"].Machine["fake"].Payload.Path
			staged := filepath.Join(t.TempDir(), "staged.sqfs")
			if err := os.Rename(path, staged); err != nil {
				t.Fatal(err)
			}
			if err := tt.replace(path, staged); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
			if tt.refused == nil {
				if err != nil {
					t.Fatal(err)
				}
				order := h.ordered(0, stagesPayload, payloadPhase, publishes)
				if got := h.machine.stdins["ws-1"][order[0]]; got != payloadBytes {
					t.Errorf("the staging read %q, want the payload file", got)
				}
				return
			}
			if want := tt.refused(path, info); err == nil || err.Error() != want {
				t.Fatalf("Create = %v, want %q", err, want)
			}
			if h.machine.ran("ws-1", stagesPayload) != 0 || h.machine.ran("ws-1", payloadPhase) != 0 || h.machine.ran("ws-1", mergesPlugins) != 0 || h.machine.ran("ws-1", installs) != 0 || h.machine.ran("ws-1", publishes) != 0 {
				t.Errorf("a create with a non-regular payload ran %q", h.machine.scripts["ws-1"])
			}
			if _, ok := h.record("ws-1"); ok {
				t.Error("the failed create kept its record")
			}
		})
	}
}

func TestResumeRemountsThePayloadOnlyWhenItsDigestChanged(t *testing.T) {
	other := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		sha256  string
		moved   bool
		remount bool
	}{
		{name: "a new digest stages and mounts the new payload", sha256: other, remount: true},
		{name: "the same digest at a new path stays ready", sha256: sha, moved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPayloadHarness(t)
			ctx := context.Background()
			if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
				t.Fatal(err)
			}
			if err := h.session.Suspend(ctx, "ws-1"); err != nil {
				t.Fatal(err)
			}
			machine := h.cfg.Profiles["lean"].Machine["fake"]
			path := machine.Payload.Path
			if tt.moved {
				moved := filepath.Join(t.TempDir(), "moved.sqfs")
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				path = moved
			}
			machine.Payload = &config.Payload{Source: config.Source{Path: path, SHA256: tt.sha256}, Packages: machine.Payload.Packages}
			h.cfg.Profiles["lean"].Machine["fake"] = machine
			repaid := h.open()
			if (repaid.Stamp != h.session.Stamp) != tt.remount {
				t.Fatalf("the payload change moved the stamp from %s to %s, want a move %v", h.session.Stamp, repaid.Stamp, tt.remount)
			}
			before := len(h.machine.scripts["ws-1"])
			if _, err := repaid.Resume(ctx, "ws-1"); err != nil {
				t.Fatal(err)
			}
			if h.machine.ready["ws-1"] != repaid.Stamp {
				t.Errorf("the resume left stamp %q, want %q", h.machine.ready["ws-1"], repaid.Stamp)
			}
			if !tt.remount {
				for _, script := range h.machine.scripts["ws-1"][before:] {
					if strings.Contains(script, stagesPayload) || strings.Contains(script, payloadPhase) || strings.Contains(script, mergesPlugins) || strings.Contains(script, publishes) {
						t.Errorf("a resume at the ready stamp ran %q", script)
					}
				}
				return
			}
			order := h.ordered(before, readies, prereqsPhase, stagesPayload+tt.sha256, payloadPhase+tt.sha256, toolsPhase, mergesPlugins+images.PayloadRoot+"/"+tt.sha256, installs+" "+images.PayloadRoot+"/"+tt.sha256, publishes, prepares, configures)
			if staged := h.machine.stdins["ws-1"][order[2]]; staged != payloadBytes {
				t.Errorf("the staging read %q on stdin, want the payload file", staged)
			}
		})
	}
}

func TestOpenRefusesAPayloadOnAProviderWithoutSpriteEnv(t *testing.T) {
	h := newPayloadHarness(t)
	h.fake.Facts = providers.Traits{Supervisor: providers.SupervisorSetsid}
	if _, err := Open(h.cfg, h.provider, "fake", "lean", h.session.Platform); err == nil || !strings.Contains(err.Error(), "sprite-env") {
		t.Errorf("Open = %v", err)
	}
	h.fake.Facts = providers.Traits{Supervisor: providers.SupervisorSpriteEnv}
	if _, err := Open(h.cfg, h.provider, "fake", "lean", h.session.Platform); err != nil {
		t.Errorf("Open on sprite-env = %v", err)
	}
}

func TestAFailedLaneCancelsItsSiblingsBeforeAnythingIsPublished(t *testing.T) {
	h := newHarness(t, true)
	h.machine.fail = errors.New("the tailnet refused the key")
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Fatalf("Create = %v", err)
	}
	if h.machine.ran("ws-1", publishes) != 0 || h.machine.ready["ws-1"] != "" {
		t.Errorf("a failed create published stamp %q", h.machine.ready["ws-1"])
	}
}

func TestAProfilePrepareWaitsForThePackagesAndTheToolInstall(t *testing.T) {
	tests := []struct {
		name    string
		empty   bool
		release []string
		fails   string
	}{
		{name: "prepare runs once the packages and then the tool install finish", release: []string{packagesPhase, installs}},
		{name: "prepare runs once the tool install and then the packages finish", release: []string{installs, packagesPhase}},
		{name: "an empty prepare runs nothing and waits for nothing", empty: true, release: []string{packagesPhase, installs}},
		{name: "a failed packages lane never prepares", release: []string{packagesPhase, installs}, fails: packagesPhase},
		{name: "a failed tool install never prepares", release: []string{installs, packagesPhase}, fails: installs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, false)
			if tt.empty {
				profile := h.cfg.Profiles["lean"]
				profile.Prepare = nil
				h.cfg.Profiles["lean"] = profile
				h.session = h.open()
			}
			var creating sync.WaitGroup
			t.Cleanup(creating.Wait)
			upstream := map[string]*hold{packagesPhase: h.machine.hold(t, packagesPhase), installs: h.machine.hold(t, installs)}
			checkingOut, preparing, configuring := h.machine.hold(t, checkout), h.machine.hold(t, prepares), h.machine.hold(t, configures)
			release := func(fragment string) {
				result := providers.Result{}
				if fragment == tt.fails {
					result = providers.Result{Stderr: []byte(fragment + " held back the create"), ExitCode: 1}
				}
				upstream[fragment].release <- result
			}
			created := make(chan error, 1)
			creating.Go(func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			})
			upstream[packagesPhase].awaitEntered(t)
			upstream[installs].awaitEntered(t)
			checkingOut.awaitEntered(t)
			checkingOut.release <- providers.Result{}
			preparing.stillParked(t, "while the packages and the tool install were still running")
			configuring.stillParked(t, "while the packages and the tool install were still running")
			release(tt.release[0])
			preparing.stillParked(t, "once only "+tt.release[0]+" had finished")
			configuring.stillParked(t, "once only "+tt.release[0]+" had finished")
			release(tt.release[1])
			if tt.fails != "" {
				if err := <-created; err == nil || !strings.Contains(err.Error(), tt.fails+" held back the create") {
					t.Fatalf("Create = %v", err)
				}
				if preparing.ran() || configuring.ran() || h.machine.ran("ws-1", publishes) != 0 {
					t.Errorf("a create whose %q failed ran %q", tt.fails, h.machine.scripts["ws-1"])
				}
				if _, ok := h.record("ws-1"); ok {
					t.Error("the failed create kept its record")
				}
				return
			}
			configuring.awaitEntered(t)
			configuring.release <- providers.Result{}
			if tt.empty {
				if err := <-created; err != nil {
					t.Fatal(err)
				}
				if preparing.ran() || h.machine.ran("ws-1", prepares) != 0 || h.machine.ran("ws-1", publishes) != 1 {
					t.Errorf("a create with no prepare steps ran %q", h.machine.scripts["ws-1"])
				}
				return
			}
			preparing.awaitEntered(t)
			preparing.release <- providers.Result{}
			if err := <-created; err != nil {
				t.Fatal(err)
			}
			h.ordered(0, checkout, prepares, publishes)
			h.ordered(0, packagesPhase, prepares)
			h.ordered(0, installs, prepares)
			h.ordered(0, packagesPhase, configures, publishes)
			h.ordered(0, installs, configures, publishes)
			if h.machine.ran("ws-1", prepares) != 1 || h.machine.ran("ws-1", publishes) != 1 {
				t.Errorf("the create prepared %d times and published %d times: %q", h.machine.ran("ws-1", prepares), h.machine.ran("ws-1", publishes), h.machine.scripts["ws-1"])
			}
		})
	}
}

func TestTheEnrollmentWaitsForTheToolInstallButNotForThePackages(t *testing.T) {
	h := newHarness(t, true)
	h.machine.tailscaleFrom = toolsPhase
	var creating sync.WaitGroup
	t.Cleanup(creating.Wait)
	packaging, installing := h.machine.hold(t, packagesPhase), h.machine.hold(t, installs)
	enrolling, configuring := h.machine.hold(t, freshens), h.machine.hold(t, configures)
	created := make(chan error, 1)
	creating.Go(func() {
		_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
		created <- err
	})
	packaging.awaitEntered(t)
	installing.awaitEntered(t)
	enrolling.stillParked(t, "while the tool install was still running")
	installing.release <- providers.Result{}
	enrolling.awaitEntered(t)
	configuring.stillParked(t, "once only the tool install had finished")
	enrolling.release <- providers.Result{}
	packaging.release <- providers.Result{}
	configuring.awaitEntered(t)
	configuring.release <- providers.Result{}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	h.ordered(0, installs, freshens, enrolls, publishes)
	h.ordered(0, packagesPhase, configures, publishes)
	if record, found := h.record("ws-1"); !found || record.Tailnet == nil || record.Tailnet.NodeID != "nNEW" || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) || h.machine.ran("ws-1", publishes) != 1 {
		t.Errorf("record %+v (found %v), bound %v, published %d times", record, found, h.bound(), h.machine.ran("ws-1", publishes))
	}
}

func TestTheEnrollmentWaitsForThePackagesWhenTheyInstallTailscale(t *testing.T) {
	tests := []struct {
		name    string
		tailnet string
	}{
		{name: "a deb installs both tailnet bins in the packages phase", tailnet: debTailnet},
		{name: "a static artifact without tailscaled leaves the daemon to apt", tailnet: cliTailnet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := build(t, true, systemHead+tt.tailnet+configureEnv, "{}", providers.Traits{})
			h.machine.tailscaleFrom = packagesPhase
			var creating sync.WaitGroup
			t.Cleanup(creating.Wait)
			packaging, installing, enrolling := h.machine.hold(t, packagesPhase), h.machine.hold(t, installs), h.machine.hold(t, freshens)
			created := make(chan error, 1)
			creating.Go(func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			})
			packaging.awaitEntered(t)
			installing.awaitEntered(t)
			installing.release <- providers.Result{}
			enrolling.stillParked(t, "while the packages that install tailscale were still running")
			packaging.release <- providers.Result{}
			enrolling.awaitEntered(t)
			enrolling.release <- providers.Result{}
			if err := <-created; err != nil {
				t.Fatal(err)
			}
			h.ordered(0, packagesPhase, freshens, enrolls, publishes)
			h.ordered(0, installs, freshens)
			if record, found := h.record("ws-1"); !found || record.Tailnet == nil || record.Tailnet.NodeID != "nNEW" || h.machine.ran("ws-1", freshens) != 1 || h.machine.ran("ws-1", publishes) != 1 {
				t.Errorf("record %+v (found %v), checked the tailnet state %d times, published %d times", record, found, h.machine.ran("ws-1", freshens), h.machine.ran("ws-1", publishes))
			}
		})
	}
}

func TestTheScriptedHostLacksTailscaleUntilItsPhaseInstallsIt(t *testing.T) {
	daemon := tailnet.Daemon{Mode: tailnet.Kernel, Supervisor: tailnet.SpriteEnv}
	tests := []struct {
		name      string
		completed []string
		want      int
	}{
		{name: "after another phase", completed: []string{images.PhaseTools}, want: 127},
		{name: "after its own phase", completed: []string{images.PhaseTools, images.PhasePackages}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &scripted{status: noState, minted: "nNEW", tailscaleFrom: packagesPhase}
			for _, phase := range tt.completed {
				if result := m.Handle("ws-1", []string{"sudo", "bash", "-s", phase}, nil); result.ExitCode != 0 {
					t.Fatalf("the %s phase exited %d", phase, result.ExitCode)
				}
			}
			result := m.Handle("ws-1", []string{"sh", "-c", daemon.FreshScript()}, nil)
			if result.ExitCode != tt.want || (tt.want == 127) != strings.Contains(string(result.Stderr), "command not found") {
				t.Errorf("the tailnet state check exited %d with %q, want exit %d", result.ExitCode, result.Stderr, tt.want)
			}
		})
	}
}

func TestTheEnrollmentDoesNotWaitForConfigure(t *testing.T) {
	h := newHarness(t, true)
	var creating sync.WaitGroup
	t.Cleanup(creating.Wait)
	configuring := h.machine.hold(t, configures)
	created := make(chan error, 1)
	creating.Go(func() {
		_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
		created <- err
	})
	configuring.awaitEntered(t)
	h.awaitBound(tailnet.Binding{NodeID: "nNEW"})
	if h.machine.ran("ws-1", enrolls) != 1 || h.machine.ran("ws-1", publishes) != 0 {
		t.Errorf("with configure still running, the create enrolled %d times and published %d times", h.machine.ran("ws-1", enrolls), h.machine.ran("ws-1", publishes))
	}
	configuring.release <- providers.Result{}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	h.ordered(0, installs, freshens, enrolls, publishes)
	h.ordered(0, configures, publishes)
	if record, found := h.record("ws-1"); !found || record.Tailnet == nil || record.Tailnet.NodeID != "nNEW" || h.machine.ran("ws-1", publishes) != 1 {
		t.Errorf("record %+v (found %v), published %d times", record, found, h.machine.ran("ws-1", publishes))
	}
}

func TestTheEnrollmentNeverStartsWhenTheToolInstallFails(t *testing.T) {
	tests := []struct {
		name  string
		fails string
	}{
		{name: "the tools phase fails", fails: toolsPhase},
		{name: "the plugin install fails", fails: installs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, true)
			var creating sync.WaitGroup
			t.Cleanup(creating.Wait)
			failing, enrolling := h.machine.hold(t, tt.fails), h.machine.hold(t, freshens)
			created := make(chan error, 1)
			creating.Go(func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			})
			failing.awaitEntered(t)
			enrolling.stillParked(t, "before the tool install had finished")
			failing.release <- providers.Result{Stderr: []byte(tt.fails + " held back the create"), ExitCode: 1}
			if err := <-created; err == nil || !strings.Contains(err.Error(), tt.fails+" held back the create") {
				t.Fatalf("Create = %v", err)
			}
			if enrolling.ran() || h.machine.ran("ws-1", enrolls) != 0 || h.machine.ran("ws-1", logsOut) != 0 || h.machine.ran("ws-1", publishes) != 0 || h.api.mintedKeys() != 0 || len(h.api.deletedNodes()) != 0 || h.bound() != (tailnet.Binding{}) {
				t.Errorf("a create whose %q failed ran %q, minted %d keys, deleted %v, bound %v", tt.fails, h.machine.scripts["ws-1"], h.api.mintedKeys(), h.api.deletedNodes(), h.bound())
			}
			if _, found := h.record("ws-1"); found {
				t.Error("the failed create kept its record")
			}
		})
	}
}

func TestAConfigureFailureAfterTheEnrollmentStartedLeavesTheTailnet(t *testing.T) {
	tests := []struct {
		name     string
		inFlight bool
	}{
		{name: "configure fails while the enrollment is in flight", inFlight: true},
		{name: "configure fails after the enrollment finished"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, true)
			var creating sync.WaitGroup
			t.Cleanup(creating.Wait)
			configuring := h.machine.hold(t, configures)
			var enrolling *hold
			if tt.inFlight {
				enrolling = h.machine.hold(t, enrolls)
			}
			created := make(chan error, 1)
			creating.Go(func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			})
			configuring.awaitEntered(t)
			if tt.inFlight {
				enrolling.awaitEntered(t)
				if h.bound() != (tailnet.Binding{Unresolved: true}) {
					t.Fatalf("bound %v while the enroll script was running", h.bound())
				}
			} else {
				h.awaitBound(tailnet.Binding{NodeID: "nNEW"})
			}
			configuring.release <- providers.Result{Stderr: []byte("configure held back the create"), ExitCode: 1}
			if tt.inFlight {
				h.machine.setStatus(running("nNEW"))
				enrolling.release <- providers.Result{Stdout: []byte(running("nNEW"))}
			}
			if err := <-created; err == nil || !strings.Contains(err.Error(), "configure held back the create") {
				t.Fatalf("Create = %v", err)
			}
			h.ordered(0, enrolls, logsOut)
			if deleted := h.api.deletedNodes(); len(deleted) != 1 || deleted[0] != "nNEW" || h.machine.ran("ws-1", logsOut) != 1 || h.machine.ran("ws-1", publishes) != 0 || h.bound() != (tailnet.Binding{}) {
				t.Errorf("deleted %v, ran %q, bound %v", deleted, h.machine.scripts["ws-1"], h.bound())
			}
			if _, err := h.fake.Get(context.Background(), "ws-1"); !errors.Is(err, providers.ErrNotFound) {
				t.Errorf("the machine survived the failed create: %v", err)
			}
			if _, found := h.record("ws-1"); found {
				t.Error("the failed create left a record")
			}
		})
	}
}

func TestResumeProvisionsAnEnrolledHostInPlace(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	drifted := h.drift(strings.NewReplacer("1.8.2", "1.8.3", sha, strings.Repeat("ab", 32)).Replace(inventory))
	drifted.Enroller.Connect = h.session.Enroller.Connect
	before, minted := len(h.machine.scripts["ws-1"]), h.api.mintedKeys()
	result, err := drifted.Resume(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	h.ordered(before, readies, packagesPhase, publishes)
	order := h.ordered(before, readies, toolsPhase, stages, installs, publishes, prepares, configures)
	after := strings.Join(h.machine.scripts["ws-1"][before:], "\n")
	for _, kept := range []string{freshens, enrolls, logsOut, renews, checkout} {
		if strings.Contains(after, kept) {
			t.Errorf("the resume ran %q on a host that keeps its tailnet node and session state", kept)
		}
	}
	if !strings.Contains(h.machine.stdins["ws-1"][order[1]], "jq-1.8.3") || h.machine.ready["ws-1"] != drifted.Stamp {
		t.Errorf("the resume provisioned with %q and left stamp %q", h.machine.stdins["ws-1"][order[1]], h.machine.ready["ws-1"])
	}
	if h.machine.currentStatus() != running("nNEW") || h.bound() != (tailnet.Binding{NodeID: "nNEW"}) || h.api.mintedKeys() != minted || len(h.api.deletedNodes()) != 0 {
		t.Errorf("the resume disturbed the tailnet: status %s, bound %v, minted %d, deleted %v", h.machine.currentStatus(), h.bound(), h.api.mintedKeys(), h.api.deletedNodes())
	}
	if result.Tailnet == nil || result.Tailnet.NodeID != "nNEW" {
		t.Errorf("result = %+v", result)
	}
}

func TestResumeProvisionsInPlaceWhenTheInventoryDrifted(t *testing.T) {
	h := newHarness(t, false)
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
	h.ordered(before, readies, prereqsPhase, packagesPhase, publishes)
	h.ordered(before, prereqsPhase, toolsPhase)
	order := h.ordered(before, readies, toolsPhase, stages, installs, publishes, prepares, configures)
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
	if h.machine.ran("ws-1", provisions) != 0 || h.machine.ran("ws-1", installs) != 0 || h.machine.ran("ws-1", images.ManifestPath) != 1 {
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
	if after := h.machine.scripts["ws-1"][before:]; len(after) != 0 {
		t.Errorf("the refused resume ran %q", after)
	}
	if h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the refused resume moved the stamp to %q", h.machine.ready["ws-1"])
	}
}

func TestResumeRefusesWhenOnlyTheImageReferenceChanged(t *testing.T) {
	h := newImagedHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	moved := h.reimage("agent-host-v2")
	before := len(h.machine.scripts["ws-1"])
	_, err := moved.Resume(ctx, "ws-1")
	if err == nil || !strings.Contains(err.Error(), "destroy ws-1 and create it again") || !strings.Contains(err.Error(), `"agent-host-v2"`) {
		t.Fatalf("Resume = %v", err)
	}
	if after := h.machine.scripts["ws-1"][before:]; len(after) != 0 {
		t.Errorf("the refused resume ran %q", after)
	}
	if record, found := h.record("ws-1"); !found || record.Image != "agent-host" {
		t.Errorf("the refused resume rewrote the image binding: %+v, %v", record, found)
	}
}

func TestResumeRefusesWhenOnlyTheToolsDriftedOnAnImageHost(t *testing.T) {
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
	_, err := drifted.Resume(ctx, "ws-1")
	if err == nil || !strings.Contains(err.Error(), "destroy ws-1 and create it again") {
		t.Fatalf("Resume = %v", err)
	}
	if after := h.machine.scripts["ws-1"][before:]; len(after) != 0 {
		t.Errorf("the refused resume ran %q", after)
	}
}

func TestCreateAdoptsTheBakedImageWithoutInstalling(t *testing.T) {
	h := newImagedHarness(t)
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	order := h.ordered(0, images.ManifestPath, stages, configures, publishes)
	h.ordered(0, images.ManifestPath, checkout)
	for _, fragment := range []string{installs, provisions, readies} {
		if got := h.machine.ran("ws-1", fragment); got != 0 {
			t.Errorf("the adopted image ran %q %d times", fragment, got)
		}
	}
	if h.machine.ran("ws-1", stages) != 1 || h.machine.ran("ws-1", publishes) != 1 || h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the create staged %d times, published %d times, and left stamp %q, want %q", h.machine.ran("ws-1", stages), h.machine.ran("ws-1", publishes), h.machine.ready["ws-1"], h.session.Stamp)
	}
	if staged := h.machine.stdins["ws-1"][order[1]]; staged != strings.TrimSpace(string(h.session.Scripts.Plugins)) {
		t.Error("the create staged another plugins.sh than the one the image baked")
	}
	if got := h.machine.ran("ws-1", "finalize.sh"); got != 0 {
		t.Errorf("a workspace create ran the image finalizer %d times", got)
	}
}

func TestCreateFromAnOldImageReferenceNeverPublishesTheNewStamp(t *testing.T) {
	h := newImagedHarness(t)
	layered := h.drift(strings.Replace(imaged, "  workspaceDir: /workspaces\n", "  workspaceDir: /workspaces\n  layer: [\"RUN true\"]\n", 1))
	if layered.Scripts.Fingerprint() != h.session.Scripts.Fingerprint() {
		t.Fatal("the layer change moved the tool fingerprint")
	}
	ctx := context.Background()
	_, err := layered.Create(ctx, "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), `image "agent-host", which profile lean cannot adopt`) || !strings.Contains(err.Error(), "with image inputs") {
		t.Fatalf("Create = %v", err)
	}
	if got := h.machine.ran("ws-1", publishes); got != 0 || h.machine.ready["ws-1"] != "" {
		t.Errorf("the refused create published %d times and left stamp %q", got, h.machine.ready["ws-1"])
	}
	for _, fragment := range []string{stages, installs, checkout, configures} {
		if got := h.machine.ran("ws-1", fragment); got != 0 {
			t.Errorf("the refused create ran %q %d times", fragment, got)
		}
	}
	if _, err := h.fake.Get(ctx, "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the refused create left its machine: %v", err)
	}
}

func TestCreateRefusesAnImageItCannotAdopt(t *testing.T) {
	inventory, err := images.Load(newImagedHarness(t).inventory)
	if err != nil {
		t.Fatal(err)
	}
	render := func(edit func(*images.Inventory), profile, platform string) string {
		t.Helper()
		inv := inventory
		image := *inv.Image
		inv.Image = &image
		edit(&inv)
		rendered, err := images.RenderImage(inv, profile, platform)
		if err != nil {
			t.Fatal(err)
		}
		return string(rendered.Manifest)
	}
	same := func(*images.Inventory) {}
	tests := []struct {
		name, baked, want string
	}{
		{"no manifest", "", "carries no baked manifest at " + images.ManifestPath},
		{"another profile", render(same, "full", images.DefaultPlatform), `baked as profile "full"`},
		{"another platform", render(same, "lean", "linux/arm64"), `baked as profile "lean" on linux/arm64`},
		{"another base", render(func(i *images.Inventory) { i.Image.Base = "ubuntu:26.04@sha256:" + sha }, "lean", images.DefaultPlatform), `baked as profile "lean" on linux/amd64 with image inputs`},
		{"another layer", render(func(i *images.Inventory) { i.Image.Layer = []string{"RUN true"} }, "lean", images.DefaultPlatform), `baked as profile "lean" on linux/amd64 with image inputs`},
		{"other tools", render(func(i *images.Inventory) { i.Prepare = []string{"echo drifted"} }, "lean", images.DefaultPlatform), `baked as profile "lean" on linux/amd64 with image inputs`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newImagedHarness(t)
			h.machine.baked = tt.baked
			ctx := context.Background()
			_, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"})
			if err == nil || !strings.Contains(err.Error(), "profile lean cannot adopt") || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "cc-remote images build --profile lean") {
				t.Fatalf("Create = %v", err)
			}
			for _, fragment := range []string{stages, installs, checkout, configures, publishes, enrolls} {
				if got := h.machine.ran("ws-1", fragment); got != 0 {
					t.Errorf("the refused create ran %q %d times", fragment, got)
				}
			}
			if _, err := h.fake.Get(ctx, "ws-1"); !errors.Is(err, providers.ErrNotFound) {
				t.Errorf("the refused create left its machine: %v", err)
			}
			if _, found := h.record("ws-1"); found {
				t.Error("the refused create kept its record")
			}
		})
	}
}

func TestResumeKeepsAnAdoptedImageAtItsStamp(t *testing.T) {
	h := newImagedHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	before := len(h.machine.scripts["ws-1"])
	if _, err := h.session.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	after := strings.Join(h.machine.scripts["ws-1"][before:], "\n")
	if !strings.Contains(after, readies) || strings.Contains(after, images.ManifestPath) || strings.Contains(after, stages) || strings.Contains(after, installs) {
		t.Errorf("the same-stamp resume ran %q", after)
	}
	h.machine.mu.Lock()
	h.machine.ready["ws-1"] = ""
	h.machine.mu.Unlock()
	before = len(h.machine.scripts["ws-1"])
	if _, err := h.session.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	h.ordered(before, readies, images.ManifestPath, stages, publishes)
	if strings.Contains(strings.Join(h.machine.scripts["ws-1"][before:], "\n"), installs) || h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the stampless resume installed, or left stamp %q", h.machine.ready["ws-1"])
	}
}

func TestDestroyKeepsAnUnlabelledMachineAndItsRecord(t *testing.T) {
	for name, created := range map[string]func() time.Time{
		"made an hour earlier":    func() time.Time { return time.Now().Add(-time.Hour) },
		"made during the attempt": time.Now,
		"with no creation time":   func() time.Time { return time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, false)
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
			if record, found := h.record("ws-1"); !found || !record.Unverified {
				t.Fatalf("an ambiguous create dropped its accounting or left the record verified: %+v, %v", record, found)
			}
			h.session.Provider = h.provider
			if _, err := h.session.Resume(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "never confirmed") {
				t.Fatalf("Resume of an unverified record = %v", err)
			}
			if err := h.session.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "without a label proving this workspace state made it") {
				t.Fatalf("Destroy = %v", err)
			}
			if _, err := h.fake.Get(ctx, "ws-1"); err != nil {
				t.Error("destroy removed a machine of unknown ownership")
			}
			if strings.Contains(h.calls(), "destroy") {
				t.Errorf("destroy called the provider: %s", h.calls())
			}
			if record, found := h.record("ws-1"); !found || !record.Unverified {
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

func TestDestroyReportsAMachineThatSurvivesItsOwnDestroy(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	h.session.Provider = &destroyIgnored{Provider: h.provider}
	if err := h.session.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "still at the provider") {
		t.Fatalf("Destroy = %v", err)
	}
	if _, found := h.record("ws-1"); !found {
		t.Error("a destroy that could not prove absence released the name")
	}
}

func TestDestroyOfAMachineAlreadyGoneRetiresWithoutAProviderDestroy(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.fake.Destroy(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	calls := len(h.fake.Calls())
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if after := verbs(h.fake.Calls()[calls:]); strings.Contains(after, "destroy") {
		t.Errorf("destroy of an absent machine ran %q at the provider", after)
	}
	if _, found := h.record("ws-1"); found {
		t.Error("the absent machine kept its record or accounting")
	}
}

type destroyIgnored struct {
	providers.Provider
}

func (d *destroyIgnored) Destroy(context.Context, string) error {
	return nil
}

func TestAPartialCreateKeepsItsRecordUntilDestroyVerifiesTheMachine(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	h.session.Provider = &createdThenFailed{Provider: h.provider}
	_, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "configure-ssh") || !strings.Contains(err.Error(), "run destroy ws-1") {
		t.Fatalf("err = %v", err)
	}
	if _, found := h.record("ws-1"); !found {
		t.Fatal("the record of a machine that may exist was forgotten")
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

func TestConnectWritesTheSSHFragmentOrcaResolvesByTheWorkspaceName(t *testing.T) {
	h := newHarness(t, false)
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

func TestAFreshCreateRefusesAnAlreadyEnrolledImage(t *testing.T) {
	h := newHarness(t, true)
	h.machine.joined = true
	if _, err := h.session.Create(t.Context(), "ws-1", Source{Ref: "main"}); err == nil || !strings.Contains(err.Error(), "rebuild it unenrolled") {
		t.Fatalf("Create = %v", err)
	}
	if h.api.mintedKeys() != 0 || h.machine.ran("ws-1", enrolls) != 0 {
		t.Error("the already enrolled image was enrolled again")
	}
}

func TestDestroyRefusesALegacySpareOwnershipLabel(t *testing.T) {
	h := newHarness(t, false)
	ctx := t.Context()
	if _, err := h.fake.Create(ctx, providers.Spec{Name: "legacy", Labels: map[string]string{"cc-remote/spare": "old-fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	h.saveRecord(Record{Name: "ws-1", Provider: "fake", Profile: "lean", Machine: "legacy"})
	if err := h.session.Destroy(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), "without a label proving this workspace state made it") {
		t.Fatalf("Destroy = %v", err)
	}
	if _, err := h.fake.Get(ctx, "legacy"); err != nil {
		t.Fatal("the legacy resource was removed")
	}
	if _, found := h.record("ws-1"); !found {
		t.Fatal("the legacy workspace record was removed")
	}
}

func TestTheClosureLoaderRunsOnceAfterThePackagesAndTheToolInstall(t *testing.T) {
	tests := []struct {
		name    string
		release []string
	}{
		{name: "the packages finish first", release: []string{packagesPhase, installs}},
		{name: "the tool install finishes first", release: []string{installs, packagesPhase}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPayloadHarness(t)
			var creating sync.WaitGroup
			t.Cleanup(creating.Wait)
			upstream := map[string]*hold{packagesPhase: h.machine.hold(t, packagesPhase), installs: h.machine.hold(t, installs)}
			loading, preparing, configuring := h.machine.hold(t, loaderPhase), h.machine.hold(t, prepares), h.machine.hold(t, configures)
			created := make(chan error, 1)
			creating.Go(func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			})
			upstream[packagesPhase].awaitEntered(t)
			upstream[installs].awaitEntered(t)
			loading.stillParked(t, "while the packages and the tool install were still running")
			upstream[tt.release[0]].release <- providers.Result{}
			loading.stillParked(t, "once only "+tt.release[0]+" had finished")
			upstream[tt.release[1]].release <- providers.Result{}
			loading.awaitEntered(t)
			preparing.stillParked(t, "while the loader was still registering the closure")
			configuring.stillParked(t, "while the loader was still registering the closure")
			loading.release <- providers.Result{}
			preparing.awaitEntered(t)
			preparing.release <- providers.Result{}
			configuring.awaitEntered(t)
			configuring.release <- providers.Result{}
			if err := <-created; err != nil {
				t.Fatal(err)
			}
			h.ordered(0, stagesDebs+packagesSHA, packagesPhase+" resident "+packagesSHA, loaderPhase, prepares, publishes)
			h.ordered(0, payloadPhase+sha+" ", installs, loaderPhase, configures, publishes)
			if h.machine.ran("ws-1", loaderPhase) != 1 || h.machine.ran("ws-1", packagesPhase+" resident") != 1 || h.machine.ran("ws-1", packagesPhase+" full") != 0 {
				t.Errorf("the create ran the loader %d times and the resident packages %d times: %q", h.machine.ran("ws-1", loaderPhase), h.machine.ran("ws-1", packagesPhase+" resident"), h.machine.scripts["ws-1"])
			}
		})
	}
}

func TestOpenRefusesAPayloadWithoutAptPayload(t *testing.T) {
	h := newPayloadHarness(t)
	if err := os.WriteFile(h.cfg.Inventory, []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "profile lean: machine fake mounts a payload, which needs apt.payload in the inventory; only a closure payload is admitted and mounted"
	if _, err := Open(h.cfg, h.provider, "fake", "lean", h.session.Platform); err == nil || err.Error() != want {
		t.Errorf("Open = %v, want %q", err, want)
	}
	if staged := h.machine.ran("ws-1", stagesDebs) + h.machine.ran("ws-1", fetchesDebs); staged != 0 {
		t.Errorf("a payload without apt.payload transferred a packages archive %d times", staged)
	}
}

func TestTheResidentPackagesInstallWhileThePayloadStreams(t *testing.T) {
	h := newPayloadHarness(t)
	var creating sync.WaitGroup
	t.Cleanup(creating.Wait)
	streaming, packaging := h.machine.hold(t, stagesPayload), h.machine.hold(t, packagesPhase)
	created := make(chan error, 1)
	creating.Go(func() {
		_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
		created <- err
	})
	streaming.awaitEntered(t)
	packaging.awaitEntered(t)
	if mounts := h.machine.ran("ws-1", payloadPhase); mounts != 0 {
		t.Errorf("the payload mounted %d times while its stream was still held", mounts)
	}
	packaging.release <- providers.Result{}
	streaming.release <- providers.Result{}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	staged := h.ordered(0, stagesDebs+packagesSHA, packagesPhase+" resident "+packagesSHA, payloadPhase+sha+" ", loaderPhase)
	if stage := h.machine.scripts["ws-1"][staged[0]]; !strings.HasPrefix(stage, "sudo bash -c ") || !strings.HasSuffix(stage, " "+stagesDebs+packagesSHA) {
		t.Errorf("the packages archive was staged by %q", stage)
	}
	if got := h.machine.stdins["ws-1"][staged[0]]; got != packagesBytes {
		t.Errorf("staged the packages archive from %q, want %q", got, packagesBytes)
	}
	if mount := h.machine.scripts["ws-1"][staged[2]]; mount != payloadPhase+sha+" "+h.session.Scripts.Fingerprint()+" "+packagesSHA {
		t.Errorf("the payload phase ran as %q, want it to carry the packages pin", mount)
	}
	h.ordered(0, stagesPayload+sha, payloadPhase+sha+" ", loaderPhase)
	if h.machine.ran("ws-1", stagesDebs) != 1 || h.machine.ran("ws-1", fetchesDebs) != 0 || h.machine.ran("ws-1", packagesPhase+" resident") != 1 {
		t.Errorf("the create ran %q", h.machine.scripts["ws-1"])
	}
}

func TestADirectPackagesArchiveIsFetchedOverStdinWithoutTheURLLeaking(t *testing.T) {
	h, recorded := newDirectPackagesHarness(t, "printf '%s\\n' '"+packagesURL+"'\nprintf 'presigned %s\\n' '"+packagesURL+"' >&2")
	var logs, stderr bytes.Buffer
	h.session.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	h.session.Stderr = &stderr
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	order := h.ordered(0, prereqsPhase, fetchesDebs, packagesPhase+" resident "+packagesSHA, loaderPhase, publishes)
	size := strconv.Itoa(len(packagesBytes))
	if fetch := h.machine.scripts["ws-1"][order[1]]; !strings.HasPrefix(fetch, "sudo bash -c ") || !strings.HasSuffix(fetch, " "+fetchesDebs+packagesSHA+" "+size) {
		t.Errorf("the packages archive was fetched by %q", fetch)
	}
	if got := h.machine.stdins["ws-1"][order[1]]; got != `url = "`+packagesURL+`"` {
		t.Errorf("the fetch read %q on stdin, want the curl config carrying the URL", got)
	}
	if h.machine.ran("ws-1", stagesDebs) != 0 || h.machine.ran("ws-1", fetchesDebs) != 1 || h.machine.ran("ws-1", stagesPayload) != 1 || h.machine.ran("ws-1", fetchesPayload) != 0 || h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the archive was staged %d times and fetched %d times, the payload was staged %d times and fetched %d times, and the stamp is %q", h.machine.ran("ws-1", stagesDebs), h.machine.ran("ws-1", fetchesDebs), h.machine.ran("ws-1", stagesPayload), h.machine.ran("ws-1", fetchesPayload), h.machine.ready["ws-1"])
	}
	for i, script := range h.machine.scripts["ws-1"] {
		if strings.Contains(script, urlSentinel) || (i != order[1] && strings.Contains(h.machine.stdins["ws-1"][i], urlSentinel)) {
			t.Errorf("exec %d carries the packages URL outside the fetch's stdin: %q", i, script)
		}
	}
	args, err := os.ReadFile(recorded + ".args")
	if err != nil {
		t.Fatal(err)
	}
	if want := packagesSHA + "\n" + size + "\n"; string(args) != want {
		t.Errorf("the url command ran with %q, want %q", args, want)
	}
	env, err := os.ReadFile(recorded + ".env")
	if err != nil {
		t.Fatal(err)
	}
	marshalled, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(h.session.State.Workspace("ws-1"))
	if err != nil {
		t.Fatal(err)
	}
	ssh, err := os.ReadFile(h.session.State.SSH("ws-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"fetched the packages archive"`) {
		t.Errorf("the log lacks the fetch milestone:\n%s", logs.String())
	}
	for name, text := range map[string]string{"the url command's environment": string(env), "the log": logs.String(), "stderr": stderr.String(), "the result": string(marshalled), "the workspace record": string(record), "the ssh fragment": string(ssh)} {
		if strings.Contains(text, urlSentinel) {
			t.Errorf("%s carries the packages URL", name)
		}
	}
}

func TestAFailedPackagesURLCommandFailsTheCreateBeforeTheMachine(t *testing.T) {
	h, _ := newDirectPackagesHarness(t, "printf 'aws s3 presign %s failed\\n' '"+packagesURL+"' >&2\nexit 1")
	var logs, stderr bytes.Buffer
	h.session.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	h.session.Stderr = &stderr
	_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if want := "packages archive url_command ./packages-url: exit status 1"; err == nil || err.Error() != want {
		t.Fatalf("Create = %v, want %q", err, want)
	}
	if stderr.Len() != 0 || len(h.fake.Calls()) != 0 {
		t.Errorf("the failed url command reached stderr %q or the provider %q", stderr.String(), h.fake.Calls())
	}
	if _, ok := h.record("ws-1"); ok {
		t.Error("the failed create kept its record")
	}
	for name, text := range map[string]string{"the error": err.Error(), "the log": logs.String(), "stderr": stderr.String()} {
		if strings.Contains(text, urlSentinel) {
			t.Errorf("%s carries the packages URL", name)
		}
	}
}

func TestTheClosureNeedsItsPackagesArchivePin(t *testing.T) {
	tests := []struct {
		name     string
		tools    string
		packages *config.Source
		want     string
	}{
		{name: "a closure payload without a packages pin", tools: inventory + closureApt, want: "profile lean: machine fake mounts a payload whose inventory declares apt.payload, so it installs the resident packages from the archive its payload build wrote; pin it under payload.packages"},
		{name: "a packages pin without a closure", tools: inventory, packages: &config.Source{Path: "debs.tar", SHA256: packagesSHA}, want: "profile lean: machine fake mounts a payload, which needs apt.payload in the inventory; only a closure payload is admitted and mounted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := build(t, false, tt.tools, "{}", providers.Traits{Supervisor: providers.SupervisorSpriteEnv})
			machine := h.cfg.Profiles["lean"].Machine["fake"]
			machine.Payload = &config.Payload{Source: config.Source{Path: "agent.sqfs", SHA256: sha}, Packages: tt.packages}
			h.cfg.Profiles["lean"].Machine["fake"] = machine
			if _, err := Open(h.cfg, h.provider, "fake", "lean", h.session.Platform); err == nil || err.Error() != tt.want {
				t.Errorf("Open = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAResumeInstallsTheResidentPackagesFromTheNewArchive(t *testing.T) {
	next, nextPackages := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	h := newPayloadHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "next.sqfs")
	if err := os.WriteFile(path, []byte(payloadBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := h.cfg.Profiles["lean"].Machine["fake"]
	machine.Payload = &config.Payload{Source: config.Source{Path: path, SHA256: next}, Packages: &config.Source{Path: machine.Payload.Packages.Path, SHA256: nextPackages}}
	h.cfg.Profiles["lean"].Machine["fake"] = machine
	resumed := h.open()
	before := len(h.machine.scripts["ws-1"])
	if _, err := resumed.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	h.ordered(before, readies, stagesDebs+nextPackages, packagesPhase+" resident "+nextPackages, loaderPhase, publishes)
	h.ordered(before, readies, stagesPayload+next, payloadPhase+next+" "+resumed.Scripts.Fingerprint()+" "+nextPackages, loaderPhase, publishes)
}

func TestAResumeRemountsAndRejectsAPackagesPinTheReadyPayloadWasNotBuiltWith(t *testing.T) {
	other := strings.Repeat("cd", 32)
	h := newPayloadHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	machine := h.cfg.Profiles["lean"].Machine["fake"]
	pin := *machine.Payload.Packages
	pin.SHA256 = other
	machine.Payload = &config.Payload{Source: machine.Payload.Source, Packages: &pin}
	h.cfg.Profiles["lean"].Machine["fake"] = machine
	mispaired := h.open()
	if mispaired.Stamp == h.session.Stamp {
		t.Fatalf("pinning another packages archive left the stamp at %s", h.session.Stamp)
	}
	rejection := fmt.Sprintf("cc-remote: payload %s/%s has packages %q, want %q", images.PayloadRoot, sha, packagesSHA, other)
	mounting := h.machine.hold(t, payloadPhase+sha+" "+mispaired.Scripts.Fingerprint()+" "+other)
	mounting.release <- providers.Result{Stderr: []byte(rejection), ExitCode: 1}
	before := len(h.machine.scripts["ws-1"])
	if _, err := mispaired.Resume(ctx, "ws-1"); err == nil || !strings.Contains(err.Error(), rejection) {
		t.Fatalf("Resume = %v, want the mount to reject the packages pin with %q", err, rejection)
	}
	if !mounting.ran() {
		t.Fatalf("the resume trusted the ready stamp instead of mounting against the new packages pin: %q", h.machine.scripts["ws-1"][before:])
	}
	h.ordered(before, readies, payloadPhase+sha+" ")
	if h.machine.ready["ws-1"] != h.session.Stamp {
		t.Errorf("the rejected resume moved the ready stamp from %s to %q", h.session.Stamp, h.machine.ready["ws-1"])
	}
	for _, script := range h.machine.scripts["ws-1"][before:] {
		if strings.Contains(script, publishes) || strings.Contains(script, loaderPhase) {
			t.Errorf("the rejected resume ran %q", script)
		}
	}
}
