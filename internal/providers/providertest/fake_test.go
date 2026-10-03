package providertest

import (
	"bytes"
	"slices"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

func TestFakeSatisfiesTheContract(t *testing.T) {
	Run(t, func(t *testing.T) Harness {
		return Harness{
			Provider: &Fake{
				Facts: providers.Traits{TailnetMode: providers.TailnetKernel, Supervisor: providers.SupervisorSpriteEnv},
				Handle: func(_ string, cmd []string, stdin []byte) providers.Result {
					result, err := providers.OSRunner{}.Run(t.Context(), providers.Command{Name: cmd[0], Args: cmd[1:], Stdin: bytes.NewReader(stdin)})
					if err != nil {
						t.Fatalf("running %v: %v", cmd, err)
					}
					return result
				},
			},
			Spec: func(name string, labels map[string]string) providers.Spec {
				return providers.Spec{Name: name, Profile: "agents", Labels: labels}
			},
			TracksState: true,
		}
	})
}

func TestFakeWithComputeAccessSatisfiesTheContract(t *testing.T) {
	Run(t, func(t *testing.T) Harness {
		return Harness{
			Provider: &Fake{
				Facts:   providers.Traits{TailnetMode: providers.TailnetUserspace, Supervisor: providers.SupervisorSetsid},
				Compute: &providers.ComputeInstance{Container: "agent", Endpoint: "https://compute.test", ContainerPort: 18766},
				Handle: func(_ string, cmd []string, stdin []byte) providers.Result {
					result, err := providers.OSRunner{}.Run(t.Context(), providers.Command{Name: cmd[0], Args: cmd[1:], Stdin: bytes.NewReader(stdin)})
					if err != nil {
						t.Fatalf("running %v: %v", cmd, err)
					}
					return result
				},
			},
			Spec: func(name string, labels map[string]string) providers.Spec {
				return providers.Spec{Name: name, Profile: "agents", Labels: labels}
			},
			TracksState: true,
		}
	})
}

func TestFakeRecordsCalls(t *testing.T) {
	fake := &Fake{}
	ctx := t.Context()
	if _, err := fake.Create(ctx, providers.Spec{Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Suspend(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.Exec(ctx, "alpha", []string{"echo", "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := fake.Destroy(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	want := []string{"create alpha", "suspend alpha", `exec alpha ["echo" "hi"]`, "destroy alpha"}
	if got := fake.Calls(); !slices.Equal(got, want) {
		t.Errorf("Calls() = %q, want %q", got, want)
	}
}
