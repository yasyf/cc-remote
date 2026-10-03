package namespace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"namespacelabs.dev/integrations/api"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
	"namespacelabs.dev/integrations/proto/namespace/stdlib"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	Name             = "namespace"
	DefaultCLI       = "nsc"
	DefaultContainer = "agent"
	nameLimit        = 63
	readyPoll        = 5 * time.Second
	hostMount        = "/volumes/volume0"
	listPage         = 100
	labelName        = "cc-remote.name"
)

var (
	nativeLabel = regexp.MustCompile(`^[a-z]([a-z0-9-.]*[a-z0-9])?$`)
	shapeSize   = regexp.MustCompile(`^([1-9][0-9]*)x([1-9][0-9]*)$`)
	pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

type Config struct {
	CLI          string        `yaml:"cli"`
	Endpoint     string        `yaml:"endpoint"`
	StateDir     string        `yaml:"-"`
	Platform     string        `yaml:"platform"`
	Container    string        `yaml:"container"`
	ExportPort   int32         `yaml:"exportPort"`
	VolumeSizeGB int           `yaml:"volumeSizeGB"`
	Duration     time.Duration `yaml:"duration"`
	CallTimeout  time.Duration `yaml:"callTimeout"`
	ReadyTimeout time.Duration `yaml:"readyTimeout"`
}

func (c Config) validate() error {
	osName, arch, ok := strings.Cut(c.Platform, "/")
	switch {
	case c.CLI == "" || c.Container == "" || !filepath.IsAbs(c.StateDir):
		return errors.New("namespace needs a cli, a container name, and an absolute state directory")
	case !strings.HasPrefix(c.Endpoint, "https://"):
		return fmt.Errorf("namespace endpoint %q must be the https regional Compute API endpoint every lifecycle call uses", c.Endpoint)
	case !ok || osName == "" || arch == "" || strings.Contains(arch, "/"):
		return fmt.Errorf("namespace platform %q must be os/arch, like linux/amd64", c.Platform)
	case c.ExportPort <= 0 || c.ExportPort > 65535:
		return fmt.Errorf("namespace exportPort %d must be the container port the Orca runtime listens on", c.ExportPort)
	case c.VolumeSizeGB <= 0 || c.Duration <= 0:
		return errors.New("namespace needs a positive volumeSizeGB and a positive duration, the finite lifetime each instance is created with")
	case c.CallTimeout <= 0 || c.ReadyTimeout <= 0:
		return errors.New("namespace needs a positive callTimeout and readyTimeout")
	}
	return nil
}

type Provider struct {
	Config
	Runner  providers.Runner
	Tokens  func() (api.TokenSource, error)
	Dial    func(ctx context.Context, endpoint string, tokens api.TokenSource) (computev1beta.ComputeServiceClient, io.Closer, error)
	Gateway Gateway
	Now     func() time.Time
	After   func(time.Duration) <-chan time.Time
}

var (
	_ providers.Provider = (*Provider)(nil)
	_ providers.Extender = (*Provider)(nil)
)

func New(config Config) (*Provider, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	silenceDiagnostics()
	return &Provider{Config: config, Runner: providers.OSRunner{}, Tokens: loadUserToken, Dial: dialCompute, Gateway: dialGateway, Now: time.Now, After: time.After}, nil
}

func (p *Provider) Traits() providers.Traits {
	return providers.Traits{
		TailnetMode: providers.TailnetUserspace,
		Supervisor:  providers.SupervisorSetsid,
		Platform:    p.Platform,
	}
}

type instance struct {
	Name     string                    `json:"name"`
	Labels   map[string]string         `json:"labels"`
	Instance providers.ComputeInstance `json:"instance"`
}

func (p *Provider) records() string { return filepath.Join(p.StateDir, Name, "instances") }

func (p *Provider) recordPath(id string) string { return filepath.Join(p.records(), id+".json") }

func (p *Provider) load(id string) (*instance, error) {
	raw, err := os.ReadFile(p.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved instance
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("%s: %w", p.recordPath(id), err)
	}
	return &saved, nil
}

func (p *Provider) save(saved instance) error {
	if err := os.MkdirAll(p.records(), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return providers.WriteFileAtomic(p.recordPath(saved.Instance.InstanceID), raw, 0o600)
}

func (p *Provider) remove(id string) error {
	if err := os.Remove(p.recordPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (p *Provider) owned() ([]instance, error) {
	entries, err := os.ReadDir(p.records())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []instance
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		saved, err := p.load(id)
		if err != nil {
			return nil, err
		}
		if saved == nil {
			continue
		}
		all = append(all, *saved)
	}
	return all, nil
}

func (p *Provider) client(ctx context.Context, endpoint string) (computev1beta.ComputeServiceClient, io.Closer, error) {
	tokens, err := p.Tokens()
	if err != nil {
		return nil, nil, err
	}
	return p.Dial(ctx, endpoint, tokens)
}

func (p *Provider) call(ctx context.Context, endpoint string, do func(context.Context, computev1beta.ComputeServiceClient) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, p.CallTimeout)
	defer cancel()
	compute, closer, err := p.client(ctx, endpoint)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closer.Close()) }()
	return do(ctx, compute)
}

func (p *Provider) Check(context.Context) error {
	if _, err := exec.LookPath(p.CLI); err != nil {
		return fmt.Errorf("nsc CLI %q not found; install it from https://namespace.so/docs/reference/cli: %w", p.CLI, err)
	}
	if _, err := p.Tokens(); err != nil {
		return fmt.Errorf("not logged in to Namespace; run 'nsc login': %w", err)
	}
	return nil
}

func (p *Provider) ValidateSpec(spec providers.Spec) error {
	if _, _, err := shapeOf(spec.Size); err != nil {
		return err
	}
	switch {
	case spec.Image == "":
		return errors.New("a namespace spec needs an image")
	case !pinnedImage.MatchString(spec.Image):
		return fmt.Errorf("namespace image %q must be pinned by its @sha256 digest", spec.Image)
	}
	return nil
}

func shapeOf(size string) (vcpus, memoryMB int32, err error) {
	match := shapeSize.FindStringSubmatch(size)
	if match == nil {
		return 0, 0, fmt.Errorf("a namespace spec needs a size of <vcpu>x<memory GB>, like 8x16, not %q", size)
	}
	cpu, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("namespace size %q asks for more than the %d vCPUs a Compute shape can carry", size, math.MaxInt32)
	}
	gb, err := strconv.ParseInt(match[2], 10, 32)
	if err != nil || gb > math.MaxInt32/1024 {
		return 0, 0, fmt.Errorf("namespace size %q asks for more than the %d GB a Compute shape can carry in megabytes", size, math.MaxInt32/1024)
	}
	return int32(cpu), int32(gb) * 1024, nil
}

func (p *Provider) shape(size string) (*computev1beta.InstanceShape, error) {
	vcpus, memoryMB, err := shapeOf(size)
	if err != nil {
		return nil, err
	}
	osName, arch, _ := strings.Cut(p.Platform, "/")
	return &computev1beta.InstanceShape{VirtualCpu: vcpus, MemoryMegabytes: memoryMB, MachineArch: arch, Os: osName}, nil
}

func translate(labels map[string]string) ([]*stdlib.Label, error) {
	native := make([]*stdlib.Label, 0, len(labels))
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		translated := strings.ReplaceAll(key, "/", ".")
		if !nativeLabel.MatchString(translated) || len(translated) > 63 || translated == labelName {
			return nil, fmt.Errorf("label %q has no Namespace label name; names are lowercase letters, digits, '-' and '.'", key)
		}
		native = append(native, &stdlib.Label{Name: translated, Value: labels[key]})
	}
	return native, nil
}

func volumeTag(name string) (string, error) {
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "cc-remote-" + name + "-" + hex.EncodeToString(nonce), nil
}

func (p *Provider) request(spec providers.Spec, tag string, deadline time.Time) (*computev1beta.CreateInstanceRequest, error) {
	labels, err := translate(spec.Labels)
	if err != nil {
		return nil, err
	}
	labels = append([]*stdlib.Label{{Name: labelName, Value: spec.Name}}, labels...)
	shape, err := p.shape(spec.Size)
	if err != nil {
		return nil, err
	}
	return &computev1beta.CreateInstanceRequest{
		Shape:             shape,
		DocumentedPurpose: "cc-remote workspace " + spec.Name,
		Labels:            labels,
		Deadline:          timestamppb.New(deadline),
		Region:            spec.Region,
		Volumes: []*computev1beta.VolumeRequest{{
			MountPoint:      hostMount,
			Tag:             tag,
			SizeMb:          int64(p.VolumeSizeGB) * 1024,
			PersistencyKind: computev1beta.VolumeRequest_PERSISTENT,
		}},
		Containers: []*computev1beta.ContainerRequest{{
			Name:        p.Container,
			ImageRef:    spec.Image,
			Entrypoint:  []string{"sleep", "infinity"},
			Environment: map[string]string{"HOME": spec.Home},
			ExportPorts: []*computev1beta.ContainerPort{{Proto: computev1beta.ContainerPort_TCP, ContainerPort: p.ExportPort}},
			Experimental: &computev1beta.ContainerRequest_ExperimentalFeatures{
				HostMount: []*computev1beta.ContainerRequest_ExperimentalFeatures_HostMount{{HostPath: hostMount, ContainerPath: spec.Root}},
			},
		}},
	}, nil
}

func (p *Provider) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if err := providers.CheckName(spec.Name, nameLimit); err != nil {
		return providers.Machine{}, err
	}
	if err := p.ValidateSpec(spec); err != nil {
		return providers.Machine{}, err
	}
	if !path.IsAbs(spec.Root) || path.Clean(spec.Root) != spec.Root || !path.IsAbs(spec.Home) {
		return providers.Machine{}, fmt.Errorf("a namespace instance needs an absolute workspace root and HOME, not %q and %q", spec.Root, spec.Home)
	}
	if err := p.unclaimed(ctx, spec.Name); err != nil {
		return providers.Machine{}, err
	}
	tag, err := volumeTag(spec.Name)
	if err != nil {
		return providers.Machine{}, err
	}
	request, err := p.request(spec, tag, p.Now().Add(p.Duration))
	if err != nil {
		return providers.Machine{}, err
	}
	var created *computev1beta.DescribeInstanceResponse
	if err := p.call(ctx, p.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) (err error) {
		created, err = compute.CreateInstance(ctx, request)
		return err
	}); err != nil {
		return providers.Machine{}, fmt.Errorf("namespace instance %s: whether the create allocated one is unknown, so no instance ID was recorded: %w", spec.Name, err)
	}
	id := created.GetMetadata().GetInstanceId()
	if id == "" {
		return providers.Machine{}, fmt.Errorf("namespace instance %s: the create answered without an instance ID, so whether it allocated one is unknown", spec.Name)
	}
	saved := instance{Name: spec.Name, Labels: maps.Clone(spec.Labels), Instance: providers.ComputeInstance{
		InstanceID:    id,
		Container:     p.Container,
		Region:        spec.Region,
		Endpoint:      p.Endpoint,
		Image:         spec.Image,
		Volume:        providers.Volume{Tag: tag, HostMount: hostMount, ContainerMount: spec.Root},
		ContainerPort: int(p.ExportPort),
	}}
	project(&saved.Instance, created)
	unready := func(cause error) (providers.Machine, error) {
		machine := p.machine(saved)
		machine.State = providers.StateUnknown
		return machine, fmt.Errorf("namespace instance %s: %w: %w", id, providers.ErrAmbiguous, cause)
	}
	if err := p.save(saved); err != nil {
		return unready(fmt.Errorf("allocated but its record was not written: %w", err))
	}
	if spec.Allocated != nil {
		if err := spec.Allocated(ctx, p.machine(saved)); err != nil {
			return unready(fmt.Errorf("allocated but not retained: %w", err))
		}
	}
	if err := p.ready(ctx, &saved); err != nil {
		return unready(err)
	}
	if err := p.own(ctx, saved.Instance); err != nil {
		return unready(err)
	}
	return p.machine(saved), nil
}

func (p *Provider) unclaimed(ctx context.Context, name string) error {
	all, err := p.owned()
	if err != nil {
		return err
	}
	for _, saved := range all {
		if saved.Name != name {
			continue
		}
		switch _, err := p.describe(ctx, saved); {
		case err == nil:
			return fmt.Errorf("namespace instance %s for %s: %w", saved.Instance.InstanceID, name, providers.ErrExists)
		case !errors.Is(err, providers.ErrNotFound):
			return err
		}
	}
	return nil
}

func (p *Provider) ready(ctx context.Context, saved *instance) error {
	if err := p.wait(ctx, saved); err != nil {
		return fmt.Errorf("allocated but not ready: %w", err)
	}
	described, err := p.describe(ctx, *saved)
	if err != nil {
		return fmt.Errorf("ready but unreadable: %w", err)
	}
	project(&saved.Instance, described)
	if saved.Instance.ExportedPort == 0 {
		return fmt.Errorf("container %s exports no host port for container port %d", saved.Instance.Container, saved.Instance.ContainerPort)
	}
	return p.save(*saved)
}

func (p *Provider) wait(ctx context.Context, saved *instance) error {
	ctx, cancel := context.WithTimeout(ctx, p.ReadyTimeout)
	defer cancel()
	compute, closer, err := p.client(ctx, saved.Instance.Endpoint)
	if err != nil {
		return err
	}
	_, err = compute.WaitInstanceSync(ctx, &computev1beta.WaitInstanceRequest{InstanceId: saved.Instance.InstanceID, ContainerName: saved.Instance.Container})
	return errors.Join(err, closer.Close())
}

func project(into *providers.ComputeInstance, described *computev1beta.DescribeInstanceResponse) {
	observe(into, described.GetMetadata())
	into.ExportedPort = exported(described.GetContainers(), into.Container, into.ContainerPort)
}

func observe(into *providers.ComputeInstance, metadata *computev1beta.InstanceMetadata) {
	into.Status = metadata.GetStatus().String()
	if created := metadata.GetCreatedAt(); created != nil {
		into.CreatedAt = created.AsTime().UTC()
	}
	if deadline := metadata.GetDeadline(); deadline != nil {
		into.Deadline = deadline.AsTime().UTC()
	}
	into.IngressDomain = metadata.GetIngressDomain()
}

func exported(containers []*computev1beta.AllocatedContainer, name string, port int) int {
	for _, container := range containers {
		if container.GetName() != name {
			continue
		}
		for _, mapping := range container.GetExportedPort() {
			if int(mapping.GetContainerPort()) == port && mapping.GetProto() == computev1beta.ContainerPort_TCP {
				return int(mapping.GetExportedPort())
			}
		}
	}
	return 0
}

func (p *Provider) own(ctx context.Context, compute providers.ComputeInstance) error {
	ctx, cancel := context.WithTimeout(ctx, p.ReadyTimeout)
	defer cancel()
	err := p.initialize(ctx, compute)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the new workspace volume was not initialized within readyTimeout %s: %w", p.ReadyTimeout, err)
	}
	return err
}

func (p *Provider) initialize(ctx context.Context, compute providers.ComputeInstance) error {
	uid, err := p.number(ctx, compute, "-u")
	if err != nil {
		return err
	}
	gid, err := p.number(ctx, compute, "-g")
	if err != nil {
		return err
	}
	owner := strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
	result, err := p.Runner.Run(ctx, p.host(compute, []string{"chown", owner, compute.Volume.HostMount}, nil))
	if err != nil {
		return fmt.Errorf("giving the new workspace volume to %s: %w", owner, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("giving the new workspace volume %s to %s exited %d: %s", compute.Volume.HostMount, owner, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	result, err = p.Runner.Run(ctx, p.container(compute, []string{"test", "-w", compute.Volume.ContainerMount}, nil))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("container %s cannot write its workspace root %s after the ownership change", compute.Container, compute.Volume.ContainerMount)
	}
	return nil
}

func (p *Provider) number(ctx context.Context, compute providers.ComputeInstance, flag string) (int, error) {
	result, err := p.Runner.Run(ctx, p.container(compute, []string{"id", flag}, nil))
	if err != nil {
		return 0, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(string(result.Stdout)))
	switch {
	case result.ExitCode != 0:
		return 0, fmt.Errorf("id %s in container %s exited %d: %s", flag, compute.Container, result.ExitCode, bytes.TrimSpace(result.Stderr))
	case parseErr != nil || value < 0:
		return 0, fmt.Errorf("id %s in container %s printed %q, not a numeric id", flag, compute.Container, result.Stdout)
	case value == 0:
		return 0, fmt.Errorf("container %s runs as root; the workspace needs the image's nonroot user", compute.Container)
	}
	return value, nil
}

func (p *Provider) describe(ctx context.Context, saved instance) (*computev1beta.DescribeInstanceResponse, error) {
	var described *computev1beta.DescribeInstanceResponse
	err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) (err error) {
		described, err = compute.DescribeInstance(ctx, &computev1beta.DescribeInstanceRequest{InstanceId: saved.Instance.InstanceID})
		return err
	})
	switch {
	case status.Code(err) == codes.NotFound:
		return nil, fmt.Errorf("namespace instance %s: %w", saved.Instance.InstanceID, providers.ErrNotFound)
	case err != nil:
		return nil, err
	case described.GetMetadata().GetStatus() == computev1beta.InstanceMetadata_DESTROYED:
		return nil, fmt.Errorf("namespace instance %s was destroyed: %w", saved.Instance.InstanceID, providers.ErrNotFound)
	}
	return described, nil
}

func (p *Provider) find(ctx context.Context, id string) (instance, *computev1beta.DescribeInstanceResponse, error) {
	saved, err := p.load(id)
	if err != nil {
		return instance{}, nil, err
	}
	if saved == nil {
		return instance{}, nil, fmt.Errorf("namespace instance %s: %w; cc-remote records only the Compute instances it created, so a Devbox-era record is no instance and its Devbox, if any, is left untouched", id, providers.ErrNotFound)
	}
	described, err := p.describe(ctx, *saved)
	if err != nil {
		return *saved, nil, err
	}
	project(&saved.Instance, described)
	return *saved, described, nil
}

func state(status string) providers.State {
	switch status {
	case computev1beta.InstanceMetadata_RUNNING.String():
		return providers.StateRunning
	case computev1beta.InstanceMetadata_SUSPENDED.String(), computev1beta.InstanceMetadata_SUSPENDING.String():
		return providers.StateSuspended
	}
	return providers.StateUnknown
}

func (p *Provider) machine(saved instance) providers.Machine {
	compute := saved.Instance
	return providers.Machine{
		ID:        compute.InstanceID,
		Provider:  Name,
		State:     state(compute.Status),
		CreatedAt: compute.CreatedAt,
		Labels:    maps.Clone(saved.Labels),
		Compute:   &compute,
	}
}

func (p *Provider) Get(ctx context.Context, id string) (providers.Machine, error) {
	saved, _, err := p.find(ctx, id)
	if err != nil {
		return providers.Machine{}, err
	}
	return p.machine(saved), nil
}

func (p *Provider) List(ctx context.Context, labels map[string]string) ([]providers.Machine, error) {
	all, err := p.owned()
	if err != nil {
		return nil, err
	}
	byID := map[string]instance{}
	for _, saved := range all {
		if saved.Instance.Endpoint == p.Endpoint {
			byID[saved.Instance.InstanceID] = saved
		}
	}
	filter, err := listFilter(labels)
	if err != nil {
		return nil, err
	}
	var machines []providers.Machine
	listed := map[string]bool{}
	var cursor []byte
	for {
		var page *computev1beta.ListInstancesResponse
		if err := p.call(ctx, p.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) (err error) {
			page, err = compute.ListInstances(ctx, &computev1beta.ListInstancesRequest{PaginationCursor: cursor, MaxEntries: listPage, LabelFilter: filter})
			return err
		}); err != nil {
			return nil, err
		}
		for _, metadata := range page.GetInstances() {
			saved, ok := byID[metadata.GetInstanceId()]
			if !ok || metadata.GetStatus() == computev1beta.InstanceMetadata_DESTROYED {
				continue
			}
			listed[saved.Instance.InstanceID] = true
			observe(&saved.Instance, metadata)
			if machine := p.machine(saved); machine.HasLabels(labels) {
				machines = append(machines, machine)
			}
		}
		if cursor = page.GetPaginationCursor(); len(cursor) == 0 {
			break
		}
	}
	return p.unlisted(ctx, machines, byID, listed, labels)
}

func (p *Provider) unlisted(ctx context.Context, machines []providers.Machine, byID map[string]instance, listed map[string]bool, labels map[string]string) ([]providers.Machine, error) {
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		saved := byID[id]
		if listed[id] || !p.machine(saved).HasLabels(labels) {
			continue
		}
		described, err := p.describe(ctx, saved)
		if errors.Is(err, providers.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		project(&saved.Instance, described)
		machines = append(machines, p.machine(saved))
	}
	return machines, nil
}

func listFilter(labels map[string]string) ([]*stdlib.LabelFilterEntry, error) {
	native, err := translate(labels)
	if err != nil {
		return nil, err
	}
	filter := make([]*stdlib.LabelFilterEntry, 0, len(native))
	for _, label := range native {
		filter = append(filter, &stdlib.LabelFilterEntry{Name: label.Name, Value: label.Value, Op: stdlib.LabelFilterEntry_EQUAL})
	}
	return filter, nil
}

func (p *Provider) settle(ctx context.Context, saved instance, want ...computev1beta.InstanceMetadata_Status) error {
	ctx, cancel := context.WithTimeout(ctx, p.ReadyTimeout)
	defer cancel()
	for {
		described, err := p.describe(ctx, saved)
		if err != nil {
			return err
		}
		current := described.GetMetadata().GetStatus()
		if slices.Contains(want, current) {
			return nil
		}
		if current == computev1beta.InstanceMetadata_ERROR {
			return fmt.Errorf("namespace instance %s is in ERROR", saved.Instance.InstanceID)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace instance %s is %s after %s, not %v", saved.Instance.InstanceID, current, p.ReadyTimeout, want)
		case <-time.After(readyPoll):
		}
	}
}

func (p *Provider) Wake(ctx context.Context, id string) error {
	saved, described, err := p.find(ctx, id)
	if err != nil {
		return err
	}
	switch described.GetMetadata().GetStatus() {
	case computev1beta.InstanceMetadata_RUNNING:
		return nil
	case computev1beta.InstanceMetadata_SUSPENDED, computev1beta.InstanceMetadata_SUSPENDING:
	default:
		return fmt.Errorf("namespace instance %s is %s, which wake does not resume", id, described.GetMetadata().GetStatus())
	}
	if err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) error {
		_, err := compute.WakeInstance(ctx, &computev1beta.WakeInstanceRequest{InstanceId: id})
		return err
	}); err != nil {
		return fmt.Errorf("waking namespace instance %s: %w", id, err)
	}
	return p.settle(ctx, saved, computev1beta.InstanceMetadata_RUNNING)
}

func (p *Provider) Suspend(ctx context.Context, id string) error {
	saved, described, err := p.find(ctx, id)
	if err != nil {
		return err
	}
	switch described.GetMetadata().GetStatus() {
	case computev1beta.InstanceMetadata_SUSPENDED:
		return nil
	case computev1beta.InstanceMetadata_RUNNING:
	default:
		return fmt.Errorf("namespace instance %s is %s, which suspend does not snapshot", id, described.GetMetadata().GetStatus())
	}
	if err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) error {
		_, err := compute.SuspendInstance(ctx, &computev1beta.SuspendInstanceRequest{InstanceId: id})
		return err
	}); err != nil {
		return fmt.Errorf("suspending namespace instance %s: %w", id, err)
	}
	return p.settle(ctx, saved, computev1beta.InstanceMetadata_SUSPENDED)
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	saved, _, err := p.find(ctx, id)
	if errors.Is(err, providers.ErrNotFound) && saved.Instance.InstanceID != "" {
		return errors.Join(err, p.remove(id))
	}
	if err != nil {
		return err
	}
	if err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) error {
		_, err := compute.DestroyInstance(ctx, &computev1beta.DestroyInstanceRequest{InstanceId: id, Reason: "cc-remote destroy " + saved.Name})
		return err
	}); err != nil {
		return fmt.Errorf("destroying namespace instance %s: %w", id, err)
	}
	if err := p.settle(ctx, saved, computev1beta.InstanceMetadata_DESTROYED); !errors.Is(err, providers.ErrNotFound) {
		return fmt.Errorf("destroying namespace instance %s: %w", id, err)
	}
	return p.remove(id)
}

func (p *Provider) Extend(ctx context.Context, id string, by time.Duration) (time.Time, error) {
	if by <= 0 {
		return time.Time{}, fmt.Errorf("extend namespace instance %s by %s: the extension must be positive", id, by)
	}
	saved, _, err := p.find(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	var extended *computev1beta.ExtendInstanceResponse
	if err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) (err error) {
		extended, err = compute.ExtendInstance(ctx, &computev1beta.ExtendInstanceRequest{InstanceId: id, ExtendBy: durationpb.New(by)})
		return err
	}); err != nil {
		return time.Time{}, fmt.Errorf("extending namespace instance %s by %s: %w", id, by, err)
	}
	if extended.GetNewDeadline() == nil {
		return time.Time{}, fmt.Errorf("extending namespace instance %s answered without its new deadline", id)
	}
	saved.Instance.Deadline = extended.GetNewDeadline().AsTime().UTC()
	return saved.Instance.Deadline, p.save(saved)
}

func (p *Provider) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	saved, err := p.load(id)
	if err != nil {
		return providers.Result{}, err
	}
	if saved == nil {
		return providers.Result{}, fmt.Errorf("namespace instance %s: %w", id, providers.ErrNotFound)
	}
	result, err := p.Runner.Run(ctx, p.container(saved.Instance, cmd, stdin))
	if err != nil || result.ExitCode == 0 {
		return result, err
	}
	if _, err := p.describe(ctx, *saved); errors.Is(err, providers.ErrNotFound) {
		return providers.Result{}, err
	}
	return result, nil
}

func (p *Provider) Access(ctx context.Context, id string) (providers.Access, error) {
	saved, _, err := p.find(ctx, id)
	if err != nil {
		return providers.Access{}, err
	}
	return providers.Access{Kind: providers.AccessCompute, Compute: saved.Instance}, nil
}
