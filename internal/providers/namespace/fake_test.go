package namespace

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"namespacelabs.dev/integrations/api"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
	"namespacelabs.dev/integrations/proto/namespace/stdlib"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	testEndpoint = "https://compute.test"
	testDomain   = "us.test.nscluster.cloud"
	testImage    = "nscr.io/tenant/agent@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	testPort     = 18766
	firstHost    = 30000
)

var epoch = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

type fakeTokens struct{}

func (fakeTokens) IssueToken(context.Context, time.Duration, bool) (string, error) {
	return "test-token", nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

type fakeInstance struct {
	request  *computev1beta.CreateInstanceRequest
	metadata *computev1beta.InstanceMetadata
	exported int32
}

type fakeNamespace struct {
	computev1beta.ComputeServiceClient
	t     *testing.T
	clock *fakeClock

	createErr   error
	anonymous   bool
	waitErr     error
	unexported  bool
	describeErr error
	lingers     bool
	extendErr   error
	clamp       time.Time
	limit       time.Duration
	latency     time.Duration
	readyAfter  time.Duration
	ownAfter    time.Duration
	stall       string
	page        int
	uid, gid    string
	chownExit   int
	readonly    bool

	mu        sync.Mutex
	instances map[string]*fakeInstance
	calls     []string
	created   []*computev1beta.CreateInstanceRequest
	extended  []*computev1beta.ExtendInstanceRequest
	shells    [][]string
	envs      [][]string
	dials     []string
	open      int
}

func newFakeNamespace(t *testing.T, clock *fakeClock) *fakeNamespace {
	return &fakeNamespace{t: t, clock: clock, page: 2, uid: "1000", gid: "1000", instances: map[string]*fakeInstance{}}
}

func (f *fakeNamespace) granted(wanted time.Time) time.Time {
	if f.limit > 0 && wanted.After(f.clock.Now().Add(f.limit)) {
		return f.clock.Now().Add(f.limit)
	}
	return wanted
}

func (f *fakeNamespace) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeNamespace) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeNamespace) Shells() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.shells)
}

func (f *fakeNamespace) Envs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.envs)
}

func (f *fakeNamespace) set(id string, to computev1beta.InstanceMetadata_Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id].metadata.Status = to
}

func (f *fakeNamespace) dial(_ context.Context, endpoint string, tokens api.TokenSource) (computev1beta.ComputeServiceClient, io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tokens == nil {
		f.t.Errorf("dialed %s without the loaded user token", endpoint)
	}
	f.dials = append(f.dials, endpoint)
	f.open++
	return f, closerFunc(func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.open--
		return nil
	}), nil
}

func (f *fakeNamespace) lookup(id string) (*fakeInstance, error) {
	instance, ok := f.instances[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", id)
	}
	return instance, nil
}

func (f *fakeNamespace) response(id string) *computev1beta.DescribeInstanceResponse {
	instance := f.instances[id]
	response := &computev1beta.DescribeInstanceResponse{Metadata: proto.Clone(instance.metadata).(*computev1beta.InstanceMetadata)}
	if !f.unexported {
		container := instance.request.GetContainers()[0]
		response.Containers = []*computev1beta.AllocatedContainer{{
			Name: container.GetName(),
			ExportedPort: []*computev1beta.AllocatedContainer_ExportedContainerPort{{
				Proto:         computev1beta.ContainerPort_TCP,
				ContainerPort: container.GetExportPorts()[0].GetContainerPort(),
				ExportedPort:  instance.exported,
			}},
		}}
	}
	return response
}

func (f *fakeNamespace) CreateInstance(_ context.Context, in *computev1beta.CreateInstanceRequest, _ ...grpc.CallOption) (*computev1beta.DescribeInstanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create")
	f.created = append(f.created, proto.Clone(in).(*computev1beta.CreateInstanceRequest))
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.anonymous {
		return &computev1beta.DescribeInstanceResponse{Metadata: &computev1beta.InstanceMetadata{}}, nil
	}
	n := len(f.instances)
	id := fmt.Sprintf("inst%d", n+1)
	f.instances[id] = &fakeInstance{
		request:  proto.Clone(in).(*computev1beta.CreateInstanceRequest),
		exported: int32(firstHost + n),
		metadata: &computev1beta.InstanceMetadata{
			InstanceId:    id,
			CreatedAt:     timestamppb.New(epoch.Add(time.Duration(n) * time.Second)),
			Deadline:      timestamppb.New(f.granted(in.GetDeadline().AsTime())),
			Status:        computev1beta.InstanceMetadata_CREATING,
			IngressDomain: testDomain,
			Labels:        in.GetLabels(),
		},
	}
	return f.response(id), nil
}

func (f *fakeNamespace) WaitInstanceSync(_ context.Context, in *computev1beta.WaitInstanceRequest, _ ...grpc.CallOption) (*computev1beta.WaitInstanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("wait %s %s", in.GetInstanceId(), in.GetContainerName())
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	instance, err := f.lookup(in.GetInstanceId())
	if err != nil {
		return nil, err
	}
	f.clock.advance(f.readyAfter)
	instance.metadata.Status = computev1beta.InstanceMetadata_RUNNING
	return &computev1beta.WaitInstanceResponse{}, nil
}

func (f *fakeNamespace) DescribeInstance(_ context.Context, in *computev1beta.DescribeInstanceRequest, _ ...grpc.CallOption) (*computev1beta.DescribeInstanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("describe %s", in.GetInstanceId())
	f.clock.advance(f.latency)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if _, err := f.lookup(in.GetInstanceId()); err != nil {
		return nil, err
	}
	return f.response(in.GetInstanceId()), nil
}

var listedByDefault = []computev1beta.InstanceMetadata_Status{
	computev1beta.InstanceMetadata_PENDING,
	computev1beta.InstanceMetadata_CREATING,
	computev1beta.InstanceMetadata_RUNNING,
}

func (f *fakeNamespace) ListInstances(_ context.Context, in *computev1beta.ListInstancesRequest, _ ...grpc.CallOption) (*computev1beta.ListInstancesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list %s", in.GetPaginationCursor())
	if in.GetMaxEntries() != listPage || in.GetIncludeCompleteRuns() {
		f.t.Errorf("list request %v, want pages of %d live instances", in, listPage)
	}
	var matched []*computev1beta.InstanceMetadata
	for _, id := range slices.Sorted(maps.Keys(f.instances)) {
		metadata := f.instances[id].metadata
		if slices.Contains(listedByDefault, metadata.GetStatus()) && f.matches(metadata.GetLabels(), in.GetLabelFilter()) {
			matched = append(matched, proto.Clone(metadata).(*computev1beta.InstanceMetadata))
		}
	}
	start := 0
	if cursor := in.GetPaginationCursor(); len(cursor) > 0 {
		var err error
		if start, err = strconv.Atoi(string(cursor)); err != nil {
			f.t.Errorf("cursor %q is not one the fake issued", cursor)
		}
	}
	end := min(start+f.page, len(matched))
	page := &computev1beta.ListInstancesResponse{Instances: matched[start:end]}
	if end < len(matched) {
		page.PaginationCursor = []byte(strconv.Itoa(end))
	}
	return page, nil
}

func (f *fakeNamespace) matches(labels []*stdlib.Label, filter []*stdlib.LabelFilterEntry) bool {
	for _, entry := range filter {
		if entry.GetOp() != stdlib.LabelFilterEntry_EQUAL {
			f.t.Errorf("label filter %v is not an equality", entry)
		}
		if !slices.ContainsFunc(labels, func(label *stdlib.Label) bool {
			return label.GetName() == entry.GetName() && label.GetValue() == entry.GetValue()
		}) {
			return false
		}
	}
	return true
}

func (f *fakeNamespace) DestroyInstance(_ context.Context, in *computev1beta.DestroyInstanceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("destroy %s %s", in.GetInstanceId(), in.GetReason())
	instance, err := f.lookup(in.GetInstanceId())
	if err != nil {
		return nil, err
	}
	instance.metadata.Status = computev1beta.InstanceMetadata_DESTROYED
	if f.lingers {
		instance.metadata.Status = computev1beta.InstanceMetadata_DESTROYING
	}
	return &emptypb.Empty{}, nil
}

func (f *fakeNamespace) SuspendInstance(_ context.Context, in *computev1beta.SuspendInstanceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("suspend %s", in.GetInstanceId())
	instance, err := f.lookup(in.GetInstanceId())
	if err != nil {
		return nil, err
	}
	instance.metadata.Status = computev1beta.InstanceMetadata_SUSPENDED
	return &emptypb.Empty{}, nil
}

func (f *fakeNamespace) WakeInstance(_ context.Context, in *computev1beta.WakeInstanceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("wake %s", in.GetInstanceId())
	instance, err := f.lookup(in.GetInstanceId())
	if err != nil {
		return nil, err
	}
	instance.metadata.Status = computev1beta.InstanceMetadata_RUNNING
	instance.exported++
	return &emptypb.Empty{}, nil
}

func (f *fakeNamespace) ExtendInstance(_ context.Context, in *computev1beta.ExtendInstanceRequest, _ ...grpc.CallOption) (*computev1beta.ExtendInstanceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extended = append(f.extended, proto.Clone(in).(*computev1beta.ExtendInstanceRequest))
	switch {
	case in.GetExtendBy() != nil && in.GetEnsureMinimum() == nil && in.GetNewDeadline() == nil:
		f.record("extend %s by %s", in.GetInstanceId(), in.GetExtendBy().AsDuration())
	case in.GetEnsureMinimum() != nil && in.GetExtendBy() == nil && in.GetNewDeadline() == nil:
		f.record("extend %s to at least %s", in.GetInstanceId(), in.GetEnsureMinimum().AsDuration())
	default:
		f.t.Errorf("extend request %v sets other than exactly one of extend_by and ensure_minimum", in)
	}
	if f.extendErr != nil {
		return nil, f.extendErr
	}
	instance, err := f.lookup(in.GetInstanceId())
	if err != nil {
		return nil, err
	}
	current := instance.metadata.GetDeadline().AsTime()
	wanted := current.Add(in.GetExtendBy().AsDuration())
	if minimum := in.GetEnsureMinimum(); minimum != nil {
		wanted = f.clock.Now().Add(minimum.AsDuration())
	}
	if wanted = f.granted(wanted); !f.clamp.IsZero() && wanted.After(f.clamp) {
		wanted = f.clamp
	}
	if wanted.After(current) {
		instance.metadata.Deadline = timestamppb.New(wanted)
	}
	f.clock.advance(f.latency)
	return &computev1beta.ExtendInstanceResponse{NewDeadline: instance.metadata.GetDeadline()}, nil
}

func (f *fakeNamespace) Run(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	f.mu.Lock()
	f.shells = append(f.shells, slices.Clone(cmd.Args))
	f.envs = append(f.envs, slices.Clone(cmd.Env))
	if cmd.Name != DefaultCLI {
		f.mu.Unlock()
		return providers.Result{}, fmt.Errorf("%s: %w", cmd.Name, exec.ErrNotFound)
	}
	args, container := cmd.Args, ""
	switch {
	case len(args) == 6 && args[0] == "ssh" && args[1] == "--container_name" && args[3] == "-T":
		container, args = args[2], args[4:]
	case len(args) == 4 && args[0] == "ssh" && args[1] == "-T":
		args = args[2:]
	default:
		f.mu.Unlock()
		f.t.Errorf("nsc %q is not a native ssh command", cmd.Args)
		return providers.Result{ExitCode: 2}, nil
	}
	id, script := args[0], args[1]
	instance, ok := f.instances[id]
	if !ok || instance.metadata.GetStatus() != computev1beta.InstanceMetadata_RUNNING {
		f.mu.Unlock()
		return providers.Result{Stderr: []byte("Failed: instance " + id + " is not running\n"), ExitCode: 255}, nil
	}
	if want := instance.request.GetContainers()[0].GetName(); container != "" && container != want {
		f.t.Errorf("ssh into container %q, want %q", container, want)
	}
	uid, gid, chownExit, readonly, stall := f.uid, f.gid, f.chownExit, f.readonly, f.stall
	f.mu.Unlock()
	switch {
	case script == stall:
		<-ctx.Done()
		return providers.Result{}, fmt.Errorf("%s: %w", cmd.Name, ctx.Err())
	case container == "":
		return providers.Result{ExitCode: chownExit}, nil
	case script == "id -u":
		f.clock.advance(f.ownAfter)
		return providers.Result{Stdout: []byte(uid + "\n")}, nil
	case script == "id -g":
		return providers.Result{Stdout: []byte(gid + "\n")}, nil
	case strings.HasPrefix(script, "test -w "):
		if readonly {
			return providers.Result{ExitCode: 1}, nil
		}
		return providers.Result{}, nil
	}
	return providers.OSRunner{}.Run(ctx, providers.Command{Name: "sh", Args: []string{"-c", script}, Env: cmd.Env, Stdin: cmd.Stdin, Stdout: cmd.Stdout})
}

func newProvider(t *testing.T) (*Provider, *fakeNamespace, *fakeClock) {
	t.Helper()
	p, err := New(Config{
		CLI:          DefaultCLI,
		Endpoint:     testEndpoint,
		StateDir:     t.TempDir(),
		Platform:     "linux/amd64",
		Container:    DefaultContainer,
		ExportPort:   testPort,
		VolumeSizeGB: 125,
		Duration:     4 * time.Hour,
		CallTimeout:  time.Minute,
		ReadyTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: epoch}
	fake := newFakeNamespace(t, clock)
	p.Runner, p.Dial, p.Now = fake, fake.dial, clock.Now
	p.Tokens = func() (api.TokenSource, error) { return fakeTokens{}, nil }
	p.Gateway = func(_ context.Context, _ io.Writer, _ api.TokenSource, endpoint string) (net.Conn, error) {
		t.Errorf("unexpected gateway dial to %s", endpoint)
		return nil, fmt.Errorf("no gateway in this test")
	}
	p.After = func(time.Duration) <-chan time.Time {
		t.Error("unexpected lease wait")
		return nil
	}
	t.Cleanup(func() {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.open != 0 {
			t.Errorf("%d Compute clients were left open", fake.open)
		}
	})
	return p, fake, clock
}

func spec(name string, labels map[string]string) providers.Spec {
	return providers.Spec{Name: name, Profile: "agents", Size: "8x16", Image: testImage, Root: "/workspaces", Home: "/home/agent", Labels: labels}
}
