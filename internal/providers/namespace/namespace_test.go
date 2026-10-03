package namespace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"namespacelabs.dev/integrations/api"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
	"namespacelabs.dev/integrations/proto/namespace/stdlib"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

func TestNamespaceSatisfiesTheContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Harness {
		p, _, _ := newProvider(t)
		return providertest.Harness{Provider: p, Spec: spec, TracksState: true}
	})
}

func TestCreateSendsTheTypedRequestAndRecordsTheReturnedInstance(t *testing.T) {
	p, fake, _ := newProvider(t)
	request := spec("alpha", map[string]string{"cc-remote/workspace": "alpha", "team": "a"})
	request.Region = "us-east-1"
	machine, err := p.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("create requests = %d", len(fake.created))
	}
	got := fake.created[0]
	tag := got.GetVolumes()[0].GetTag()
	if !regexp.MustCompile(`^cc-remote-alpha-[0-9a-f]{12}$`).MatchString(tag) {
		t.Errorf("volume tag %q is not a new unique tag for alpha", tag)
	}
	want := &computev1beta.CreateInstanceRequest{
		Shape:             &computev1beta.InstanceShape{VirtualCpu: 8, MemoryMegabytes: 16384, MachineArch: "amd64", Os: "linux"},
		DocumentedPurpose: "cc-remote workspace alpha",
		Labels: []*stdlib.Label{
			{Name: "cc-remote.name", Value: "alpha"},
			{Name: "cc-remote.workspace", Value: "alpha"},
			{Name: "team", Value: "a"},
		},
		Deadline: timestamppb.New(epoch.Add(4 * time.Hour)),
		Region:   "us-east-1",
		Volumes: []*computev1beta.VolumeRequest{{
			MountPoint:      "/volumes/volume0",
			Tag:             tag,
			SizeMb:          125 * 1024,
			PersistencyKind: computev1beta.VolumeRequest_PERSISTENT,
		}},
		Containers: []*computev1beta.ContainerRequest{{
			Name:        "agent",
			ImageRef:    testImage,
			Entrypoint:  []string{"sleep", "infinity"},
			Environment: map[string]string{"HOME": "/home/agent"},
			ExportPorts: []*computev1beta.ContainerPort{{Proto: computev1beta.ContainerPort_TCP, ContainerPort: testPort}},
			Experimental: &computev1beta.ContainerRequest_ExperimentalFeatures{
				HostMount: []*computev1beta.ContainerRequest_ExperimentalFeatures_HostMount{{HostPath: "/volumes/volume0", ContainerPath: "/workspaces"}},
			},
		}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("create request =\n%v\nwant\n%v", got, want)
	}
	compute := providers.ComputeInstance{
		InstanceID:    "inst1",
		Container:     "agent",
		Region:        "us-east-1",
		Endpoint:      testEndpoint,
		Image:         testImage,
		Status:        "RUNNING",
		CreatedAt:     epoch,
		Deadline:      epoch.Add(4 * time.Hour),
		Volume:        providers.Volume{Tag: tag, HostMount: "/volumes/volume0", ContainerMount: "/workspaces"},
		ContainerPort: testPort,
		ExportedPort:  firstHost,
		IngressDomain: testDomain,
	}
	if machine.ID != "inst1" || machine.Provider != Name || machine.State != providers.StateRunning || machine.Compute == nil || *machine.Compute != compute {
		t.Errorf("machine = %+v, compute %+v; want %+v", machine, machine.Compute, compute)
	}
	if got := fake.Calls(); !slices.Equal(got, []string{"create", "wait inst1 agent", "describe inst1"}) {
		t.Errorf("calls = %q", got)
	}
	raw, err := os.ReadFile(p.recordPath("inst1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "test-token") || !strings.Contains(string(raw), `"instanceId":"inst1"`) {
		t.Errorf("record = %s, want the instance's safe metadata only", raw)
	}
	if info, err := os.Stat(p.recordPath("inst1")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, %v", info, err)
	}
}

func TestCreateOwnsOnlyTheNewMountWithOneNonRecursiveChown(t *testing.T) {
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ssh", "--container_name", "agent", "-T", "inst1", "id -u"},
		{"ssh", "--container_name", "agent", "-T", "inst1", "id -g"},
		{"ssh", "-T", "inst1", "chown 1000:1000 /volumes/volume0"},
		{"ssh", "--container_name", "agent", "-T", "inst1", "test -w /workspaces"},
	}
	if got := fake.Shells(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("nsc commands =\n%q\nwant\n%q", got, want)
	}
	ctx := t.Context()
	if _, err := p.Get(ctx, "inst1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.List(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Access(ctx, "inst1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Suspend(ctx, "inst1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Wake(ctx, "inst1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Extend(ctx, "inst1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := fake.Shells(); len(got) != len(want) {
		t.Errorf("lifecycle calls after create ran nsc %q, want no further ownership change", got[len(want):])
	}
}

func TestCreateWhoseAllocationIsUnknownRecordsNoID(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeNamespace)
		want  string
	}{
		{
			"the create call fails",
			func(f *fakeNamespace) {
				f.createErr = status.Error(codes.DeadlineExceeded, "context deadline exceeded")
			},
			"namespace instance alpha: whether the create allocated one is unknown, so no instance ID was recorded: rpc error: code = DeadlineExceeded desc = context deadline exceeded",
		},
		{
			"the create answers without an ID",
			func(f *fakeNamespace) { f.anonymous = true },
			"namespace instance alpha: the create answered without an instance ID, so whether it allocated one is unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, _ := newProvider(t)
			tt.setup(fake)
			machine, err := p.Create(t.Context(), spec("alpha", nil))
			if err == nil || err.Error() != tt.want || errors.Is(err, providers.ErrAmbiguous) || errors.Is(err, providers.ErrExists) {
				t.Errorf("Create = %v, want %q", err, tt.want)
			}
			if machine.ID != "" {
				t.Errorf("Create guessed machine ID %q", machine.ID)
			}
			if got := fake.Calls(); !slices.Equal(got, []string{"create"}) {
				t.Errorf("calls = %q, want the single create and no retry", got)
			}
			if entries, _ := os.ReadDir(p.records()); len(entries) != 0 {
				t.Errorf("records = %v, want none", entries)
			}
		})
	}
}

func TestCreateKeepsAKnownIDWhenInitializationFails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeNamespace)
		want  string
		host  int
	}{
		{"not ready", func(f *fakeNamespace) { f.waitErr = status.Error(codes.Unavailable, "boot failed") }, "allocated but not ready: rpc error: code = Unavailable desc = boot failed", 0},
		{"no exported port", func(f *fakeNamespace) { f.unexported = true }, "container agent exports no host port for container port 18766", 0},
		{"a root container", func(f *fakeNamespace) { f.uid = "0" }, "container agent runs as root; the workspace needs the image's nonroot user", 0},
		{"chown fails", func(f *fakeNamespace) { f.chownExit = 1 }, "giving the new workspace volume /volumes/volume0 to 1000:1000 exited 1: ", 1},
		{"the root stays unwritable", func(f *fakeNamespace) { f.readonly = true }, "container agent cannot write its workspace root /workspaces after the ownership change", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, _ := newProvider(t)
			tt.setup(fake)
			machine, err := p.Create(t.Context(), spec("alpha", nil))
			if !errors.Is(err, providers.ErrAmbiguous) || !strings.HasSuffix(err.Error(), tt.want) {
				t.Errorf("Create = %v, want ErrAmbiguous ending %q", err, tt.want)
			}
			if machine.ID != "inst1" {
				t.Errorf("machine = %+v, want the known ID inst1", machine)
			}
			saved, err := p.load("inst1")
			if err != nil || saved == nil || saved.Name != "alpha" {
				t.Errorf("record = %+v, %v; want the known instance kept", saved, err)
			}
			if slices.ContainsFunc(fake.Calls(), func(call string) bool { return strings.HasPrefix(call, "destroy") }) {
				t.Errorf("calls = %q; a failed initialization destroyed the instance", fake.Calls())
			}
			host := 0
			for _, shell := range fake.Shells() {
				if shell[1] == "-T" {
					host++
				}
			}
			if host != tt.host {
				t.Errorf("host commands = %d, want %d", host, tt.host)
			}
		})
	}
}

func TestCreateRefusesBadInputBeforeAnyCall(t *testing.T) {
	tests := []struct {
		name string
		spec providers.Spec
		want string
	}{
		{"a label with no native name", spec("alpha", map[string]string{"Team": "a"}), `label "Team" has no Namespace label name; names are lowercase letters, digits, '-' and '.'`},
		{"the reserved name label", spec("alpha", map[string]string{"cc-remote/name": "x"}), `label "cc-remote/name" has no Namespace label name; names are lowercase letters, digits, '-' and '.'`},
		{"a relative root", providers.Spec{Name: "alpha", Size: "8x16", Image: testImage, Root: "workspaces", Home: "/home/agent"}, `a namespace instance needs an absolute workspace root and HOME, not "workspaces" and "/home/agent"`},
		{"no home", providers.Spec{Name: "alpha", Size: "8x16", Image: testImage, Root: "/workspaces"}, `a namespace instance needs an absolute workspace root and HOME, not "/workspaces" and ""`},
		{"an unpinned image", providers.Spec{Name: "alpha", Size: "8x16", Image: "nscr.io/tenant/agent:latest", Root: "/workspaces", Home: "/home/agent"}, `namespace image "nscr.io/tenant/agent:latest" must be pinned by its @sha256 digest`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, _ := newProvider(t)
			if _, err := p.Create(t.Context(), tt.spec); err == nil || err.Error() != tt.want {
				t.Errorf("Create = %v, want %q", err, tt.want)
			}
			if got := fake.Calls(); len(got) != 0 {
				t.Errorf("calls = %q, want none", got)
			}
		})
	}
}

func TestCreateReusesTheNameOfADestroyedInstance(t *testing.T) {
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.set("inst1", computev1beta.InstanceMetadata_DESTROYED)
	machine, err := p.Create(t.Context(), spec("alpha", nil))
	if err != nil || machine.ID != "inst2" {
		t.Errorf("Create after the first instance was destroyed = %+v, %v", machine, err)
	}
}

func TestValidateSpec(t *testing.T) {
	p, _, _ := newProvider(t)
	tests := []struct {
		name string
		spec providers.Spec
		want string
	}{
		{"pinned", spec("a", nil), ""},
		{"named size", providers.Spec{Size: "l", Image: testImage}, `a namespace spec needs a size of <vcpu>x<memory GB>, like 8x16, not "l"`},
		{"zero vcpu", providers.Spec{Size: "0x16", Image: testImage}, `a namespace spec needs a size of <vcpu>x<memory GB>, like 8x16, not "0x16"`},
		{"no image", providers.Spec{Size: "8x16"}, "a namespace spec needs an image"},
		{"tag only", providers.Spec{Size: "8x16", Image: "cc-remote-linux"}, `namespace image "cc-remote-linux" must be pinned by its @sha256 digest`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.ValidateSpec(tt.spec)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Errorf("ValidateSpec = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestListReadsEveryPageAndKeepsSuspendedInstances(t *testing.T) {
	p, fake, _ := newProvider(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if _, err := p.Create(t.Context(), spec(name, map[string]string{"pool": "x"})); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Suspend(t.Context(), "inst3"); err != nil {
		t.Fatal(err)
	}
	fake.instances["foreign"] = &fakeInstance{
		request:  fake.instances["inst1"].request,
		metadata: &computev1beta.InstanceMetadata{InstanceId: "foreign", Status: computev1beta.InstanceMetadata_RUNNING, Labels: []*stdlib.Label{{Name: "pool", Value: "x"}}},
	}
	before := len(fake.Calls())
	machines, err := p.List(t.Context(), map[string]string{"pool": "x"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, machine := range machines {
		ids = append(ids, machine.ID)
		if machine.Compute.ExportedPort == 0 {
			t.Errorf("%s lost its exported port in the listing", machine.ID)
		}
	}
	slices.Sort(ids)
	if want := []string{"inst1", "inst2", "inst3", "inst4", "inst5"}; !slices.Equal(ids, want) {
		t.Errorf("List = %v, want %v", ids, want)
	}
	if got := fake.Calls()[before:]; !slices.Equal(got, []string{"list ", "list 2", "list 4", "describe inst3"}) {
		t.Errorf("calls = %q, want three pages and one describe of the suspended instance", got)
	}
	if machines, err := p.List(t.Context(), map[string]string{"pool": "y"}); err != nil || len(machines) != 0 {
		t.Errorf("List(pool=y) = %v, %v", machines, err)
	}
}

func TestListReadsAnEmptyAccount(t *testing.T) {
	p, _, _ := newProvider(t)
	machines, err := p.List(t.Context(), nil)
	if err != nil || len(machines) != 0 {
		t.Errorf("List = %v, %v; want none", machines, err)
	}
}

func TestDescribeMapsOnlyATypedNotFound(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		notFound bool
	}{
		{"not found", status.Error(codes.NotFound, "no such instance"), true},
		{"unavailable", status.Error(codes.Unavailable, "not found in the cache"), false},
		{"permission", status.Error(codes.PermissionDenied, "denied"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, _ := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			fake.describeErr = tt.err
			_, err := p.Get(t.Context(), "inst1")
			if errors.Is(err, providers.ErrNotFound) != tt.notFound || status.Code(err) != status.Code(tt.err) && !tt.notFound {
				t.Errorf("Get = %v, want not found %t", err, tt.notFound)
			}
		})
	}
}

func TestALegacyDevboxRecordIsNoInstance(t *testing.T) {
	p, fake, _ := newProvider(t)
	_, err := p.Get(t.Context(), "alpha")
	want := "namespace instance alpha: machine not found; cc-remote records only the Compute instances it created, so a Devbox-era record is no instance and its Devbox, if any, is left untouched"
	if !errors.Is(err, providers.ErrNotFound) || err.Error() != want {
		t.Errorf("Get = %v, want %q", err, want)
	}
	if err := p.Destroy(t.Context(), "alpha"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Destroy = %v", err)
	}
	if got := fake.Calls(); len(got) != 0 {
		t.Errorf("calls = %q, want no Compute call for a name with no instance record", got)
	}
}

func TestWakeAndSuspendCallOnlyWhenTheStateDiffers(t *testing.T) {
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, step := range []func(context.Context, string) error{p.Wake, p.Suspend, p.Suspend, p.Wake} {
		if err := step(ctx, "inst1"); err != nil {
			t.Fatal(err)
		}
	}
	var changes []string
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "wake") || strings.HasPrefix(call, "suspend") {
			changes = append(changes, call)
		}
	}
	if !slices.Equal(changes, []string{"suspend inst1", "wake inst1"}) {
		t.Errorf("state changes = %q", changes)
	}
	fake.set("inst1", computev1beta.InstanceMetadata_ERROR)
	if err := p.Wake(ctx, "inst1"); err == nil || err.Error() != "namespace instance inst1 is ERROR, which wake does not resume" {
		t.Errorf("Wake of an errored instance = %v", err)
	}
	if err := p.Suspend(ctx, "inst1"); err == nil || err.Error() != "namespace instance inst1 is ERROR, which suspend does not snapshot" {
		t.Errorf("Suspend of an errored instance = %v", err)
	}
}

func TestDestroyKeepsTheRecordUntilTheInstanceIsDestroyed(t *testing.T) {
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.lingers = true
	p.ReadyTimeout = 50 * time.Millisecond
	err := p.Destroy(t.Context(), "inst1")
	if want := "destroying namespace instance inst1: namespace instance inst1 is DESTROYING after 50ms, not [DESTROYED]"; err == nil || err.Error() != want {
		t.Errorf("Destroy = %v, want %q", err, want)
	}
	if saved, err := p.load("inst1"); err != nil || saved == nil {
		t.Fatalf("record = %+v, %v; want it kept while the instance is destroying", saved, err)
	}
	if !slices.Contains(fake.Calls(), "destroy inst1 cc-remote destroy alpha") {
		t.Errorf("calls = %q", fake.Calls())
	}
	fake.set("inst1", computev1beta.InstanceMetadata_DESTROYED)
	if err := p.Destroy(t.Context(), "inst1"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Destroy once destroyed = %v, want ErrNotFound", err)
	}
	if saved, err := p.load("inst1"); err != nil || saved != nil {
		t.Errorf("record = %+v, %v; want it removed once the instance is gone", saved, err)
	}
}

func TestExtendRecordsTheDeadlineTheProviderAnswered(t *testing.T) {
	tests := []struct {
		name  string
		clamp time.Time
		want  time.Time
	}{
		{"granted", time.Time{}, epoch.Add(6 * time.Hour)},
		{"clamped by policy", epoch.Add(5 * time.Hour), epoch.Add(5 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, _ := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			fake.clamp = tt.clamp
			deadline, err := p.Extend(t.Context(), "inst1", 2*time.Hour)
			if err != nil || !deadline.Equal(tt.want) {
				t.Errorf("Extend = %v, %v; want %v", deadline, err, tt.want)
			}
			if saved, err := p.load("inst1"); err != nil || !saved.Instance.Deadline.Equal(tt.want) {
				t.Errorf("recorded deadline = %+v, %v", saved, err)
			}
			if !slices.Contains(fake.Calls(), "extend inst1 by 2h0m0s") {
				t.Errorf("calls = %q", fake.Calls())
			}
		})
	}
	p, fake, _ := newProvider(t)
	if _, err := p.Extend(t.Context(), "inst1", 0); err == nil || err.Error() != "extend namespace instance inst1 by 0s: the extension must be positive" {
		t.Errorf("Extend by zero = %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %q", fake.Calls())
	}
}

func TestExecRunsTheNativeContainerSSH(t *testing.T) {
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	before := len(fake.Shells())
	result, err := p.Exec(t.Context(), "inst1", []string{"sh", "-c", "cat; exit 4"}, strings.NewReader("key"))
	if err != nil || string(result.Stdout) != "key" || result.ExitCode != 4 {
		t.Errorf("Exec = %q, exit %d, %v", result.Stdout, result.ExitCode, err)
	}
	if got := fake.Shells()[before]; !slices.Equal(got, []string{"ssh", "--container_name", "agent", "-T", "inst1", "sh -c 'cat; exit 4'"}) {
		t.Errorf("nsc argv = %q", got)
	}
	fake.set("inst1", computev1beta.InstanceMetadata_DESTROYED)
	if _, err := p.Exec(t.Context(), "inst1", []string{"true"}, nil); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Exec on a destroyed instance = %v, want ErrNotFound", err)
	}
}

func TestCheck(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "nsc")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		setup func(*Provider)
		want  string
	}{
		{"logged in", func(*Provider) {}, ""},
		{"no CLI", func(p *Provider) { p.CLI = "elsewhere-nsc" }, `nsc CLI "elsewhere-nsc" not found; install it from https://namespace.so/docs/reference/cli`},
		{"logged out", func(p *Provider) {
			p.Tokens = func() (api.TokenSource, error) { return nil, errors.New("no token") }
		}, "not logged in to Namespace; run 'nsc login': no token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _, _ := newProvider(t)
			p.CLI = cli
			tt.setup(p)
			err := p.Check(t.Context())
			if tt.want == "" {
				if err != nil {
					t.Errorf("Check = %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestARecordThatVanishesAfterEnumerationIsSkipped(t *testing.T) {
	p, _, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", map[string]string{"pool": "x"})); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.records(), "destroyed-meanwhile"), filepath.Join(p.records(), "gone.json")); err != nil {
		t.Fatal(err)
	}
	all, err := p.owned()
	if err != nil || len(all) != 1 || all[0].Instance.InstanceID != "inst1" {
		t.Fatalf("owned = %+v, %v; want only the record still present", all, err)
	}
	machines, err := p.List(t.Context(), map[string]string{"pool": "x"})
	if err != nil || len(machines) != 1 || machines[0].ID != "inst1" {
		t.Errorf("List = %+v, %v", machines, err)
	}
	if _, err := p.Create(t.Context(), spec("beta", nil)); err != nil {
		t.Errorf("Create beside a vanished record = %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.records(), "torn.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.owned(); err == nil || !strings.Contains(err.Error(), "torn.json") {
		t.Errorf("owned with an undecodable record = %v, want its decode error", err)
	}
}

func TestNativeCommandsUseTheSavedEndpointAndNoInheritedDiagnostics(t *testing.T) {
	t.Setenv("NSC_ENDPOINT", "https://ambient.compute.test")
	t.Setenv("NSC_GRPC_DEBUG_RESPONSES", "1")
	p, fake, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	p.Endpoint = "https://changed.compute.test"
	result, err := p.Exec(t.Context(), "inst1", []string{"sh", "-c", `printf '%s|%s|%s|%s' "$NSC_ENDPOINT" "$NS_GRPC_DEBUG" "$NSC_GRPC_DEBUG_REQUESTS" "$NSC_GRPC_DEBUG_RESPONSES"`}, nil)
	if want := testEndpoint + "|0|0|0"; err != nil || string(result.Stdout) != want {
		t.Errorf("Exec saw %q, %v; want %q", result.Stdout, err, want)
	}
	want := []string{"NS_GRPC_DEBUG=0", "NSC_GRPC_DEBUG_REQUESTS=0", "NSC_GRPC_DEBUG_RESPONSES=0", "NSC_ENDPOINT=" + testEndpoint}
	shells, envs := fake.Shells(), fake.Envs()
	hosts := 0
	for i, shell := range shells {
		if shell[1] == "-T" {
			hosts++
		}
		if !slices.Equal(envs[i], want) {
			t.Errorf("nsc %q ran with env %q, want %q", shell, envs[i], want)
		}
	}
	if len(shells) != 5 || hosts != 1 {
		t.Errorf("nsc commands = %q, want ownership setup with its one host chown and the one Exec", shells)
	}
	if got := os.Getenv("NSC_ENDPOINT"); got != "https://ambient.compute.test" {
		t.Errorf("the controller's NSC_ENDPOINT changed to %q", got)
	}
}

type handoff struct {
	machine  providers.Machine
	at       time.Time
	calls    []string
	shells   int
	recorded bool
}

func TestCreateHandsTheAllocatedInstanceToItsKeeperBeforeReadiness(t *testing.T) {
	p, fake, clock := newProvider(t)
	fake.limit = time.Minute
	fake.readyAfter = 2 * time.Minute
	fake.ownAfter = time.Minute
	request := spec("alpha", nil)
	var handed []handoff
	request.Allocated = func(_ context.Context, machine providers.Machine) error {
		saved, err := p.load(machine.ID)
		handed = append(handed, handoff{machine: machine, at: clock.Now(), calls: fake.Calls(), shells: len(fake.Shells()), recorded: err == nil && saved != nil})
		return nil
	}
	machine, err := p.Create(t.Context(), request)
	if err != nil || machine.ID != "inst1" || machine.State != providers.StateRunning {
		t.Fatalf("Create = %+v, %v", machine, err)
	}
	if len(handed) != 1 {
		t.Fatalf("the allocation was handed over %d times, want once", len(handed))
	}
	got := handed[0]
	deadline := epoch.Add(time.Minute)
	if got.machine.ID != "inst1" || got.machine.Compute == nil || got.machine.Compute.InstanceID != "inst1" || !got.machine.Compute.Deadline.Equal(deadline) || got.machine.State == providers.StateRunning {
		t.Errorf("handed %+v, compute %+v; want the exact allocated instance and its short returned deadline, not yet ready", got.machine, got.machine.Compute)
	}
	if !got.recorded || !slices.Equal(got.calls, []string{"create"}) || got.shells != 0 || !got.at.Before(deadline) {
		t.Errorf("handed after calls %q and %d nsc commands at %v; want it recorded, before WaitInstanceSync and ownership, and before %v", got.calls, got.shells, got.at, deadline)
	}
	if !clock.Now().After(deadline) {
		t.Errorf("readiness and ownership finished at %v, inside the %v deadline the case must outlast", clock.Now(), deadline)
	}
}

func TestCreateKeepsTheInstanceWhenItsKeeperCannotStart(t *testing.T) {
	p, fake, _ := newProvider(t)
	refused := errors.New("the gateway forward could not hold its listener")
	request := spec("alpha", nil)
	request.Allocated = func(context.Context, providers.Machine) error { return refused }
	machine, err := p.Create(t.Context(), request)
	if !errors.Is(err, refused) || !errors.Is(err, providers.ErrAmbiguous) || err.Error() != "namespace instance inst1: "+providers.ErrAmbiguous.Error()+": allocated but not retained: "+refused.Error() {
		t.Errorf("Create = %v, want the known instance reported ambiguous with the keeper failure", err)
	}
	if machine.ID != "inst1" || machine.Compute == nil || machine.Compute.InstanceID != "inst1" || machine.State != providers.StateUnknown {
		t.Errorf("machine = %+v, want the exact known instance in an unknown state", machine)
	}
	if saved, err := p.load("inst1"); err != nil || saved == nil {
		t.Errorf("record = %+v, %v; want the known instance kept", saved, err)
	}
	if got := fake.Calls(); !slices.Equal(got, []string{"create"}) || len(fake.Shells()) != 0 {
		t.Errorf("calls = %q and nsc %q; want no readiness, ownership, or destroy after the keeper failed", got, fake.Shells())
	}
}

func TestOwnershipInitializationIsBoundedByReadyTimeout(t *testing.T) {
	p, fake, _ := newProvider(t)
	p.ReadyTimeout = 50 * time.Millisecond
	fake.stall = "id -u"
	started := time.Now()
	machine, err := p.Create(t.Context(), spec("alpha", nil))
	if !errors.Is(err, providers.ErrAmbiguous) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the new workspace volume was not initialized within readyTimeout 50ms") {
		t.Errorf("Create = %v, want the ownership setup stopped at readyTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the stalled ownership setup held Create for %v", elapsed)
	}
	if machine.ID != "inst1" {
		t.Errorf("machine = %+v, want the known instance", machine)
	}
	for _, shell := range fake.Shells() {
		if shell[1] == "-T" {
			t.Errorf("the host chown ran after the uid probe stalled: %q", shell)
		}
	}
	if saved, err := p.load("inst1"); err != nil || saved == nil {
		t.Errorf("record = %+v, %v; want the known instance kept", saved, err)
	}
}
