package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/workspace"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestOrcaSourceDemandsSchemaTwoAndThePinnedCheckout(t *testing.T) {
	full := map[string]string{
		"ORCA_RECIPE_RESULT_SCHEMA_VERSION": "2",
		"ORCA_REPO_URL":                     "https://github.com/example/app.git",
		"ORCA_REPO_REF":                     "feature",
		"ORCA_REPO_REF_HEAD":                strings.Repeat("a", 40),
		"ORCA_REPO_BRANCH":                  "orca/ws",
	}
	source, err := orcaSource(env(full), "https://github.com/example/app")
	if err != nil || source != (workspace.Source{Ref: "feature", Head: strings.Repeat("a", 40), Branch: "orca/ws"}) {
		t.Fatalf("source = %+v, %v", source, err)
	}
	for name, change := range map[string]map[string]string{
		"no schema":        {"ORCA_RECIPE_RESULT_SCHEMA_VERSION": ""},
		"schema one":       {"ORCA_RECIPE_RESULT_SCHEMA_VERSION": "1"},
		"other repository": {"ORCA_REPO_URL": "https://github.com/example/other"},
		"no head":          {"ORCA_REPO_REF_HEAD": ""},
		"no branch":        {"ORCA_REPO_BRANCH": ""},
		"option as ref":    {"ORCA_REPO_REF": "--upload-pack=x"},
	} {
		values := map[string]string{}
		for k, v := range full {
			values[k] = v
		}
		for k, v := range change {
			values[k] = v
		}
		if _, err := orcaSource(env(values), "https://github.com/example/app"); err == nil {
			t.Errorf("%s: a create ran", name)
		}
	}
}

func TestOrcaNameComesFromTheRecipeAndInstance(t *testing.T) {
	name, err := orcaName(env(map[string]string{"ORCA_RECIPE_ID": "sprites.lean_SSH", "ORCA_VM_INSTANCE_ID": "Inst-01"}))
	if err != nil || !strings.HasPrefix(name, "orca-sprites-lean-ssh-inst-01-") || len(name) != len("orca-sprites-lean-ssh-inst-01-")+orcaDigest {
		t.Errorf("name = %q, %v", name, err)
	}
	long, err := orcaName(env(map[string]string{"ORCA_RECIPE_ID": strings.Repeat("r", 40), "ORCA_VM_INSTANCE_ID": strings.Repeat("i", 40)}))
	other, _ := orcaName(env(map[string]string{"ORCA_RECIPE_ID": strings.Repeat("r", 40), "ORCA_VM_INSTANCE_ID": strings.Repeat("i", 39) + "j"}))
	if err != nil || len(long) != 55 || long == other || long[:40] != other[:40] {
		t.Errorf("long names = %q and %q (%d), %v", long, other, len(long), err)
	}
	same, _ := orcaName(env(map[string]string{"ORCA_RECIPE_ID": "a-b", "ORCA_VM_INSTANCE_ID": "c"}))
	split, _ := orcaName(env(map[string]string{"ORCA_RECIPE_ID": "a", "ORCA_VM_INSTANCE_ID": "b-c"}))
	if same == split {
		t.Errorf("recipe and instance boundaries collapsed into one name %q", same)
	}
	if _, err := orcaName(env(map[string]string{})); err == nil {
		t.Error("a name was invented with no recipe or instance")
	}
}

func TestOrcaLifecycleVerbsReadTheResourceFromThePayload(t *testing.T) {
	name, err := orcaResource(strings.NewReader(`{"recipeResult":{"schemaVersion":2,"userData":{"provider":"sprites","resourceId":"orca-x-1"}}}`))
	if err != nil || name != "orca-x-1" {
		t.Errorf("resource = %q, %v", name, err)
	}
	if _, err := orcaResource(strings.NewReader(`{"recipeResult":{}}`)); err == nil {
		t.Error("a payload with no resource id named one")
	}
	if _, err := orcaResource(strings.NewReader("")); err == nil {
		t.Error("an empty stdin named a resource")
	}
}

func TestOrcaResultIsTheSchemaTwoProvisionedRootShape(t *testing.T) {
	result := &workspace.Result{
		SchemaVersion: workspace.SchemaVersion,
		Name:          "orca-x-1",
		Provider:      "sprites",
		Profile:       "lean",
		Machine:       "orca-x-1",
		ProjectRoot:   "/home/sprite/app",
		SSH:           workspace.SSH{Host: "orca-x-1.sprite", Port: 22, User: "sprite", IdentityFile: "/k", ProxyCommand: "cc-remote proxy orca-x-1", Options: []string{"StrictHostKeyChecking=yes"}},
		Forwards:      []workspace.Forward{{Label: "web", Port: 40001}},
	}
	var out bytes.Buffer
	if err := emit(&out, orcaResultOf(result)); err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(out.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	connection := shape["connection"].(map[string]any)
	target := connection["target"].(map[string]any)
	forwards := target["portForwards"].([]any)[0].(map[string]any)
	userData := shape["userData"].(map[string]any)
	if shape["schemaVersion"] != float64(2) || shape["checkoutMode"] != "provisioned-root" || connection["type"] != "ssh" || connection["projectRoot"] != "/home/sprite/app" {
		t.Errorf("shape = %s", out.String())
	}
	if target["label"] != "orca-x-1" || target["host"] != "orca-x-1" || target["port"] != float64(22) || target["username"] != "sprite" || target["identityFile"] != "/k" || target["proxyCommand"] != "cc-remote proxy orca-x-1" {
		t.Errorf("target = %v", target)
	}
	if forwards["localPort"] != float64(40001) || forwards["remotePort"] != float64(40001) || forwards["remoteHost"] != "localhost" || forwards["label"] != "web" {
		t.Errorf("forwards = %v", forwards)
	}
	if userData["resourceId"] != "orca-x-1" || userData["provider"] != "sprites" || userData["profile"] != "lean" {
		t.Errorf("userData = %v", userData)
	}
	if _, leaked := target["options"]; leaked {
		t.Error("ssh options leaked into Orca's strict target schema")
	}
}

func TestOrcaConnectionFlagAcceptsSSHOnly(t *testing.T) {
	if err := orcaMode(""); err != nil {
		t.Error(err)
	}
	if err := orcaMode("ssh"); err != nil {
		t.Error(err)
	}
	if err := orcaMode("server"); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("server = %v", err)
	}
	if err := orcaMode("tcp"); err == nil {
		t.Error("tcp was accepted")
	}
}
