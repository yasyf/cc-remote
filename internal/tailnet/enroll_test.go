package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/state"
)

const (
	enrolls  = ` up --auth-key="file:`
	freshens = `test ! -e "/var/lib/tailscale/tailscaled.state"`
	logsOut  = "tailscale --socket=/run/tailscale/tailscaled.sock logout"
	noState  = `{"BackendState":"NoState"}`
)

type machine struct {
	mu       sync.Mutex
	status   string
	minted   string
	fail     error
	onEnroll func()
	onStatus func()
	scripts  []string
	stdin    []string
}

func (m *machine) setStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
}

func (m *machine) currentStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *machine) Run(_ context.Context, script string, stdin io.Reader) ([]byte, error) {
	input := ""
	if stdin != nil {
		raw, _ := io.ReadAll(stdin)
		input = strings.TrimSpace(string(raw))
	}
	m.mu.Lock()
	m.scripts = append(m.scripts, script)
	m.stdin = append(m.stdin, input)
	m.mu.Unlock()
	switch {
	case strings.Contains(script, enrolls):
		if m.onEnroll != nil {
			m.onEnroll()
		}
		if m.fail != nil {
			return nil, m.fail
		}
		m.setStatus(running(m.minted))
		return []byte(m.currentStatus()), nil
	case strings.Contains(script, "status --json"):
		if m.onStatus != nil {
			m.onStatus()
		}
		return []byte(m.currentStatus()), nil
	case strings.Contains(script, logsOut):
		m.setStatus(noState)
	}
	return nil, nil
}

func (m *machine) ran(fragment string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, script := range m.scripts {
		if strings.Contains(script, fragment) {
			count++
		}
	}
	return count
}

func running(nodeID string) string {
	return `{"BackendState":"Running","Self":{"ID":"` + nodeID + `","DNSName":"ws-1.` + suffix + `.","TailscaleIPs":["100.64.0.9"]}}`
}

func loggedOut(nodeID string) string {
	return `{"BackendState":"NeedsLogin","Self":{"ID":"` + nodeID + `"}}`
}

type fakeTailnet struct {
	devices []Device
	refuse  bool
	minted  int
	deleted []string
	tokens  int
}

func (f *fakeTailnet) serve(t *testing.T) *Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			f.tokens++
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
	return &Client{Tag: tag, Base: server.URL, HTTP: server.Client(), Credential: Credential{ClientID: "k", ClientSecret: "s", Suffix: suffix}}
}

func owned(nodeID string) Device {
	return Device{NodeID: nodeID, Name: "ws-1." + suffix, Hostname: "ws-1", Tags: []string{tag}}
}

func foreign(nodeID string) Device {
	return Device{NodeID: nodeID, Name: "ws-2." + suffix, Hostname: "ws-2", Tags: []string{tag}}
}

type harness struct {
	t        *testing.T
	dir      state.Dir
	enroller *Enroller
	machine  *machine
	api      *fakeTailnet
}

func newHarness(t *testing.T, m *machine, api *fakeTailnet) *harness {
	t.Helper()
	dir := state.Dir(t.TempDir())
	client := api.serve(t)
	enroller := &Enroller{
		Connect:  func(context.Context) (*Client, error) { return client, nil },
		Bindings: Bindings{Dir: dir.Tailnet()},
		Daemon:   sprites,
		Log:      slog.New(slog.DiscardHandler),
	}
	return &harness{t: t, dir: dir, enroller: enroller, machine: m, api: api}
}

func (h *harness) hold() *state.Held {
	h.t.Helper()
	held, err := h.dir.Hold("ws-1")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(held.Release)
	return held
}

func (h *harness) bound() Binding {
	h.t.Helper()
	binding, err := h.enroller.Bindings.Read("ws-1")
	if err != nil {
		h.t.Fatal(err)
	}
	return binding
}

func (h *harness) bind(to Binding) {
	h.t.Helper()
	held, err := h.dir.Hold("ws-1")
	if err != nil {
		h.t.Fatal(err)
	}
	defer held.Release()
	if err := h.enroller.Bindings.Of(held).Transition(Binding{}, to); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) enroll() (*Node, error) {
	held, err := h.dir.Hold("ws-1")
	if err != nil {
		return nil, err
	}
	defer held.Release()
	return h.enroller.Enroll(context.Background(), held, h.machine)
}

func (h *harness) reattach(recorded *Node) (*Node, bool, error) {
	held, err := h.dir.Hold("ws-1")
	if err != nil {
		return nil, false, err
	}
	defer held.Release()
	return h.enroller.Reattach(context.Background(), held, h.machine, recorded)
}

func (h *harness) leave(recorded *Node) error {
	held, err := h.dir.Hold("ws-1")
	if err != nil {
		return err
	}
	defer held.Release()
	return h.enroller.Leave(context.Background(), held, h.machine, recorded)
}

func node(nodeID string) *Node {
	return &Node{NodeID: nodeID}
}

var (
	none   = Binding{}
	intent = Binding{Unresolved: true}
)

func boundTo(nodeID string) Binding {
	return Binding{NodeID: nodeID}
}

func TestEnrollSendsTheMintedKeyOnStdinAndRecordsTheNode(t *testing.T) {
	m := &machine{minted: "nNEW"}
	api := &fakeTailnet{devices: []Device{owned("nNEW")}}
	h := newHarness(t, m, api)
	m.onEnroll = func() {
		if h.bound() != intent {
			t.Errorf("the key reached the machine before the enrollment intent was recorded: %v", h.bound())
		}
	}
	got, err := h.enroll()
	if err != nil {
		t.Fatal(err)
	}
	if m.ran(freshens) != 1 || m.ran(enrolls) != 1 {
		t.Errorf("enroll ran %q", m.scripts)
	}
	if api.minted != 1 || m.stdin[1] != "tskey-auth-minted" {
		t.Errorf("minted %d keys, machine read %q", api.minted, m.stdin)
	}
	for _, script := range m.scripts {
		if strings.Contains(script, "tskey-auth") {
			t.Error("the key reached the machine inside a script")
		}
	}
	if want := (Node{NodeID: "nNEW", IP: "100.64.0.9", DNSName: "ws-1." + suffix, Mode: Userspace}); got == nil || *got != want {
		t.Errorf("recorded %+v", got)
	}
	if h.bound() != boundTo("nNEW") {
		t.Errorf("bound to %v", h.bound())
	}
}

func TestEnrollVerifiesTheCredentialOnceBeforeMinting(t *testing.T) {
	m := &machine{minted: "nNEW"}
	api := &fakeTailnet{devices: []Device{owned("nNEW")}}
	h := newHarness(t, m, api)
	if _, err := h.enroller.Client(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.enroll(); err != nil {
		t.Fatal(err)
	}
	if api.tokens < 2 {
		t.Errorf("the client exchanged %d tokens; want a verification plus the minting calls", api.tokens)
	}
	h.enroller.Connect = func(context.Context) (*Client, error) { return nil, errors.New("connect again") }
	if _, err := h.enroller.Client(context.Background()); err != nil {
		t.Errorf("a verified client was rebuilt: %v", err)
	}
}

func TestEnrollRefusesToStartOverAnExistingBinding(t *testing.T) {
	m := &machine{minted: "nNEW"}
	h := newHarness(t, m, &fakeTailnet{})
	h.bind(boundTo("nOLD"))
	if _, err := h.enroller.join(context.Background(), h.hold(), m); err == nil || m.ran(enrolls) != 0 || h.bound() != boundTo("nOLD") {
		t.Errorf("err = %v, ran %q, bound %v", err, m.scripts, h.bound())
	}
}

func TestAFailedMintLeavesNoBindingBehind(t *testing.T) {
	m := &machine{}
	api := &fakeTailnet{}
	h := newHarness(t, m, api)
	client := api.serve(t)
	client.Base = "http://127.0.0.1:1"
	h.enroller.client = client
	if _, err := h.enroller.join(context.Background(), h.hold(), m); err == nil || m.ran(enrolls) != 0 || h.bound() != none {
		t.Errorf("err = %v, ran %q, bound %v", err, m.scripts, h.bound())
	}
}

func TestEnrollRecordsTheNodeTheMachineReportsHoldingAfterAFailure(t *testing.T) {
	m := &machine{fail: io.ErrUnexpectedEOF, status: running("nHALF")}
	api := &fakeTailnet{devices: []Device{owned("nHALF")}}
	h := newHarness(t, m, api)
	got, err := h.enroll()
	if err == nil || !strings.Contains(err.Error(), "recorded for cleanup") {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.NodeID != "nHALF" || h.bound() != boundTo("nHALF") {
		t.Errorf("recorded %+v, bound %v", got, h.bound())
	}
	if err := h.leave(got); err != nil || len(api.deleted) != 1 || api.deleted[0] != "nHALF" || h.bound() != none {
		t.Errorf("cleanup: %v, deleted %v, bound %v", err, api.deleted, h.bound())
	}
}

func TestEnrollRefusesANodeTheTailnetSaysBelongsToAnotherWorkspace(t *testing.T) {
	m := &machine{minted: "nOTHER"}
	api := &fakeTailnet{devices: []Device{foreign("nOTHER")}}
	h := newHarness(t, m, api)
	got, err := h.enroll()
	if err == nil || !strings.Contains(err.Error(), "not the "+tag+" node of workspace ws-1") || got != nil || h.bound() != intent {
		t.Errorf("err = %v, recorded %+v, bound %v", err, got, h.bound())
	}
}

func TestEnrollKeepsTheIntentWhenTheMachineReportsNoNode(t *testing.T) {
	m := &machine{fail: io.ErrUnexpectedEOF, status: noState}
	api := &fakeTailnet{}
	h := newHarness(t, m, api)
	if got, err := h.enroll(); err == nil || !strings.Contains(err.Error(), "check the tailnet for hostname ws-1") || got != nil || h.bound() != intent {
		t.Errorf("err = %v, recorded %+v, bound %v", err, got, h.bound())
	}
	if err := h.leave(nil); err != nil || m.ran(logsOut) != 0 || len(api.deleted) != 0 || h.bound() != intent {
		t.Errorf("destroying an unresolved workspace whose daemon holds nothing: %v, ran %q, deleted %v, bound %v", err, m.scripts, api.deleted, h.bound())
	}
}

func TestLeaveResolvesAnIntentFromTheNodeTheMachineHolds(t *testing.T) {
	m := &machine{status: running("nLATE")}
	api := &fakeTailnet{devices: []Device{owned("nLATE")}}
	h := newHarness(t, m, api)
	h.bind(intent)
	if err := h.leave(nil); err != nil || m.ran(logsOut) != 1 || len(api.deleted) != 1 || api.deleted[0] != "nLATE" || h.bound() != none {
		t.Errorf("err = %v, ran %q, deleted %v, bound %v", err, m.scripts, api.deleted, h.bound())
	}
}

func TestReattachKeepsTheNodeTheMachineStillHolds(t *testing.T) {
	m := &machine{status: running("nMINE")}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	got, joined, err := h.reattach(node("nMINE"))
	if err != nil {
		t.Fatal(err)
	}
	if joined || api.minted != 0 || len(api.deleted) != 0 || m.ran(enrolls) != 0 || got == nil || got.IP != "100.64.0.9" || h.bound() != boundTo("nMINE") {
		t.Errorf("resume joined %v, minted %d, deleted %v, ran %q, recorded %+v", joined, api.minted, api.deleted, m.scripts, got)
	}
}

func TestReattachEnrollsAgainWhenTheTailnetDroppedTheNode(t *testing.T) {
	m := &machine{status: loggedOut("nMINE"), minted: "nFRESH"}
	api := &fakeTailnet{devices: []Device{owned("nMINE"), owned("nFRESH")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	got, joined, err := h.reattach(node("nMINE"))
	if err != nil {
		t.Fatal(err)
	}
	if !joined || api.minted != 1 || m.ran(enrolls) != 1 || m.ran(freshens) != 0 || m.ran(logsOut) != 1 {
		t.Errorf("re-enroll joined %v, minted %d and ran %q", joined, api.minted, m.scripts)
	}
	if got == nil || got.NodeID != "nFRESH" || h.bound() != boundTo("nFRESH") || len(api.deleted) != 1 || api.deleted[0] != "nMINE" {
		t.Errorf("recorded %+v, bound %v, deleted %v", got, h.bound(), api.deleted)
	}
}

func TestReattachEnrollsAWorkspaceCreatedBeforeTheTailnet(t *testing.T) {
	m := &machine{status: noState, minted: "nLATE"}
	api := &fakeTailnet{devices: []Device{owned("nLATE")}}
	h := newHarness(t, m, api)
	got, joined, err := h.reattach(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !joined || api.minted != 1 || len(api.deleted) != 0 || m.ran(freshens) != 0 || got == nil || got.NodeID != "nLATE" || h.bound() != boundTo("nLATE") {
		t.Errorf("joined %v, minted %d, deleted %v, ran %q, recorded %+v, bound %v", joined, api.minted, api.deleted, m.scripts, got, h.bound())
	}
}

func TestReattachResolvesAnIntentFromTheNodeTheMachineHolds(t *testing.T) {
	m := &machine{status: running("nHALF")}
	api := &fakeTailnet{devices: []Device{owned("nHALF")}}
	h := newHarness(t, m, api)
	h.bind(intent)
	got, joined, err := h.reattach(nil)
	if err != nil {
		t.Fatal(err)
	}
	if joined || api.minted != 0 || got == nil || got.NodeID != "nHALF" || h.bound() != boundTo("nHALF") {
		t.Errorf("joined %v, minted %d, recorded %+v, bound %v", joined, api.minted, got, h.bound())
	}
}

func TestReattachRefusesWhatNoBindingOrDaemonAccountsFor(t *testing.T) {
	tests := map[string]struct {
		binding  Binding
		status   string
		recorded *Node
		devices  []Device
		want     string
	}{
		"an unrecorded node the machine holds":      {none, running("nGHOST"), nil, []Device{owned("nGHOST")}, "no enrollment of this tool recorded"},
		"a record with a node but no binding":       {none, running("nMINE"), node("nMINE"), []Device{owned("nMINE")}, "no enrollment of this tool recorded"},
		"a machine holding another node than bound": {boundTo("nMINE"), running("nOTHER"), node("nMINE"), []Device{owned("nOTHER")}, "holds tailnet node nOTHER but is bound to nMINE"},
		"a machine holding a foreign node":          {boundTo("nMINE"), running("nOTHER"), node("nMINE"), []Device{foreign("nOTHER")}, "not its own"},
		"an intent the machine cannot resolve":      {intent, noState, nil, nil, "never reported a node"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &machine{status: tt.status, minted: "nNEW"}
			api := &fakeTailnet{devices: tt.devices}
			h := newHarness(t, m, api)
			if tt.binding != none {
				h.bind(tt.binding)
			}
			_, _, err := h.reattach(tt.recorded)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
			if api.minted != 0 || len(api.deleted) != 0 || m.ran(logsOut) != 0 || h.bound() != tt.binding {
				t.Errorf("minted %d, deleted %v, ran %q, bound %v", api.minted, api.deleted, m.scripts, h.bound())
			}
		})
	}
}

func TestLeaveAfterAFailedResumeRevokesOnlyTheNodeItEnrolled(t *testing.T) {
	m := &machine{status: running("nNEW")}
	api := &fakeTailnet{devices: []Device{owned("nNEW")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nNEW"))
	if err := h.leave(node("nNEW")); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "nNEW" || h.bound() != none {
		t.Errorf("deleted %v, bound %v", api.deleted, h.bound())
	}
	m.setStatus(loggedOut("nNEW"))
	api.devices = nil
	stale := node("nOLD")
	if err := h.leave(stale); err != nil || len(api.deleted) != 1 || m.ran(logsOut) != 1 {
		t.Errorf("a destroy with the record from before the failed resume: %v, deleted %v, ran %q", err, api.deleted, m.scripts)
	}
	m.minted = "nAGAIN"
	api.devices = []Device{owned("nAGAIN")}
	got, joined, err := h.reattach(stale)
	if err != nil || !joined || api.minted != 1 || len(api.deleted) != 1 || got == nil || got.NodeID != "nAGAIN" || h.bound() != boundTo("nAGAIN") {
		t.Errorf("a resume with the record from before the failed resume: %v, joined %v, minted %d, deleted %v, recorded %+v, bound %v", err, joined, api.minted, api.deleted, got, h.bound())
	}
}

func TestReattachTreatsANodeTheTailnetNoLongerHoldsAsGone(t *testing.T) {
	m := &machine{status: running("nMINE"), minted: "nFRESH"}
	api := &fakeTailnet{devices: []Device{owned("nFRESH")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	got, joined, err := h.reattach(node("nMINE"))
	if err != nil || !joined || api.minted != 1 || len(api.deleted) != 0 || got == nil || got.NodeID != "nFRESH" || h.bound() != boundTo("nFRESH") {
		t.Errorf("err = %v, joined %v, minted %d, deleted %v, recorded %+v, bound %v", err, joined, api.minted, api.deleted, got, h.bound())
	}
}

func TestLeaveLogsOutAndDeletesExactlyTheBoundNode(t *testing.T) {
	m := &machine{status: running("nMINE")}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	if err := h.leave(node("nMINE")); err != nil {
		t.Fatal(err)
	}
	if m.ran(logsOut) != 1 || len(api.deleted) != 1 || api.deleted[0] != "nMINE" || api.minted != 0 || h.bound() != none {
		t.Errorf("leave ran %q, deleted %v, minted %d, bound %v", m.scripts, api.deleted, api.minted, h.bound())
	}
	if err := h.leave(nil); err != nil || len(api.deleted) != 1 || m.ran(logsOut) != 1 {
		t.Errorf("a workspace with no node touched the tailnet: %v %v %q", err, api.deleted, m.scripts)
	}
}

func TestLeaveRevokesTheBoundNodeWhenTheRecordOmitsIt(t *testing.T) {
	m := &machine{status: running("nMINE")}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	if err := h.leave(nil); err != nil {
		t.Fatal(err)
	}
	if m.ran(logsOut) != 1 || len(api.deleted) != 1 || api.deleted[0] != "nMINE" || h.bound() != none {
		t.Errorf("ran %q, deleted %v, bound %v", m.scripts, api.deleted, h.bound())
	}
}

func TestLeaveDeletesAnExpiredBoundNodeWithoutLoggingOut(t *testing.T) {
	m := &machine{status: noState}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	if err := h.leave(node("nMINE")); err != nil || m.ran(logsOut) != 0 || len(api.deleted) != 1 || h.bound() != none {
		t.Errorf("err = %v, ran %q, deleted %v, bound %v", err, m.scripts, api.deleted, h.bound())
	}
}

func TestLeaveRefusesWhatTheBindingOrDaemonDoesNotConfirm(t *testing.T) {
	tests := map[string]struct {
		binding  Binding
		status   string
		recorded *Node
		devices  []Device
		want     string
	}{
		"a machine holding another node than bound": {boundTo("nMINE"), running("nOTHER"), node("nMINE"), []Device{owned("nMINE"), owned("nOTHER")}, "holds node nOTHER"},
		"a machine holding a foreign node":          {boundTo("nMINE"), running("nOTHER"), node("nMINE"), []Device{owned("nMINE"), foreign("nOTHER")}, "not its own"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &machine{status: tt.status}
			api := &fakeTailnet{devices: tt.devices}
			h := newHarness(t, m, api)
			h.bind(tt.binding)
			err := h.leave(tt.recorded)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
			if len(api.deleted) != 0 || m.ran(logsOut) != 0 || h.bound() != tt.binding {
				t.Errorf("deleted %v, ran %q, bound %v", api.deleted, m.scripts, h.bound())
			}
		})
	}
}

func TestLeaveNeverDeletesByARecordedNodeNoBindingRecords(t *testing.T) {
	m := &machine{status: running("nMINE")}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}}
	h := newHarness(t, m, api)
	if err := h.leave(node("nMINE")); err != nil || len(api.deleted) != 0 || len(m.scripts) != 0 || h.bound() != none || api.tokens != 0 {
		t.Errorf("err = %v, deleted %v, ran %q, bound %v, tokens %d", err, api.deleted, m.scripts, h.bound(), api.tokens)
	}
}

func TestLeaveKeepsTheBindingWhenTheTailnetRefusesTheDelete(t *testing.T) {
	m := &machine{status: running("nMINE")}
	api := &fakeTailnet{devices: []Device{owned("nMINE")}, refuse: true}
	h := newHarness(t, m, api)
	h.bind(boundTo("nMINE"))
	if err := h.leave(node("nMINE")); err == nil || !strings.Contains(err.Error(), "403") || h.bound() != boundTo("nMINE") {
		t.Errorf("err = %v, bound %v", err, h.bound())
	}
}

func TestAStaleRecordNeverSelectsTheNodeToDelete(t *testing.T) {
	m := &machine{status: running("nNEW")}
	api := &fakeTailnet{devices: []Device{owned("nNEW"), owned("nOLD")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nNEW"))
	held := h.hold()
	if err := h.enroller.Bindings.Of(held).Transition(boundTo("nOLD"), none); err == nil || h.bound() != boundTo("nNEW") {
		t.Errorf("a transition from a stale state passed: %v, bound %v", err, h.bound())
	}
	held.Release()
	got, _, err := h.reattach(node("nOLD"))
	if err != nil || api.minted != 0 || len(api.deleted) != 0 || got == nil || got.NodeID != "nNEW" {
		t.Errorf("a resume with a stale record: %v, minted %d, deleted %v, recorded %+v", err, api.minted, api.deleted, got)
	}
	if err := h.leave(node("nOLD")); err != nil || len(api.deleted) != 1 || api.deleted[0] != "nNEW" || h.bound() != none {
		t.Errorf("a destroy with a stale record: %v, deleted %v, bound %v", err, api.deleted, h.bound())
	}
}

func TestACleanupPausedAfterObservingTheDaemonHoldsOffAReplacement(t *testing.T) {
	m := &machine{status: running("nOLD"), minted: "nNEW"}
	api := &fakeTailnet{devices: []Device{owned("nOLD"), owned("nNEW")}}
	h := newHarness(t, m, api)
	h.bind(boundTo("nOLD"))
	observed, proceed := make(chan struct{}), make(chan struct{})
	var first sync.Once
	m.onStatus = func() {
		first.Do(func() {
			close(observed)
			<-proceed
		})
	}
	cleanup := make(chan error, 1)
	go func() { cleanup <- h.leave(node("nOLD")) }()
	<-observed
	resumed := make(chan error, 1)
	var got *Node
	go func() {
		var err error
		got, _, err = h.reattach(node("nOLD"))
		resumed <- err
	}()
	select {
	case err := <-resumed:
		t.Fatalf("the replacement ran while the cleanup still held the resource: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	if err := <-cleanup; err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "nOLD" || got == nil || got.NodeID != "nNEW" || h.bound() != boundTo("nNEW") {
		t.Errorf("deleted %v, recorded %+v, bound %v", api.deleted, got, h.bound())
	}
	logout, enroll := -1, -1
	for i, script := range m.scripts {
		if strings.Contains(script, logsOut) && logout < 0 {
			logout = i
		}
		if strings.Contains(script, enrolls) {
			enroll = i
		}
	}
	if logout < 0 || enroll < 0 || logout > enroll {
		t.Errorf("the cleanup's logout did not precede the replacement's enrollment: %q", m.scripts)
	}
}
