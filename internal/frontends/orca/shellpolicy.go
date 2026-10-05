package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	policySchema   = 1
	policyDeadline = 30 * time.Second
)

const ShellPolicyScript = hookLibrary + appServerLibrary + `

BASE = ("system", "user", "project")
FILTERS = ("exclude", "include_only", "filters")


def screen(index, layer, protected):
    kind = layer["name"]["type"]
    policy = layer["config"].get("shell_environment_policy", {})
    if any(name.lower() in protected for name in policy.get("set", {})):
        raise Refusal("set", kind, index)
    if any(field in policy for field in FILTERS):
        if kind not in BASE:
            raise Refusal("managed", kind, index)
        if layer["disabledReason"] is not None:
            raise Refusal("disabled", kind, index)


def effective(policy):
    if policy["filters"] is not None or policy["exclude"] is None and policy["include_only"] is None:
        return False, []
    return True, policy["exclude"] or []


def read_policy(codex, project, seconds, names):
    deadline = time.monotonic() + seconds
    home = os.path.expanduser("~")
    if not home.startswith("/") or os.path.normpath(home) != home or os.environ.get("CODEX_HOME", home + "/.codex") != home + "/.codex":
        raise Refusal("home")
    if not os.path.isabs(project) or os.path.realpath(project) != project or not os.path.isdir(project):
        raise Refusal("project")
    env = {key: value for key, value in os.environ.items() if key not in names}
    env["PATH"] = home + "/.local/bin:/usr/local/bin:" + env.get("PATH", "")
    binary = shutil.which("codex", path=env["PATH"])
    if binary is None:
        raise Refusal("codex")
    binary = os.path.realpath(binary)
    try:
        answer = subprocess.run([binary, "--version"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=max(0.001, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise Refusal("timeout", step="version")
    if answer.returncode != 0 or answer.stdout.decode(errors="replace").strip() != "codex-cli " + codex:
        raise Refusal("codex")
    server = Server(binary, env, home, deadline)
    try:
        hello = server.request("initialize", {"clientInfo": {"name": "cc_remote", "title": "cc-remote", "version": "1"}})
        if hello.get("codexHome") != home + "/.codex":
            raise Refusal("home", step="initialize")
        server.notify("initialized")
        result = server.request("config/read", {"includeLayers": True, "cwd": project})
        protected = {name.lower() for name in names}
        for index, layer in enumerate(result["layers"]):
            screen(index, layer, protected)
        legacy, exclude = effective(result["config"]["shell_environment_policy"])
    finally:
        code = server.close()
    if code is None:
        raise Refusal("timeout", step="exit")
    if code != 0:
        raise Refusal("exit", step="exit")
    return {"schema": 1, "outcome": "exact", "reason": "", "layer": "", "record": -1, "step": "", "legacy": legacy, "exclude": exclude}


def main():
    try:
        result = read_policy(sys.argv[1], sys.argv[2], float(sys.argv[3]), set(sys.argv[4:]))
    except Refusal as refusal:
        result = {"schema": 1, "outcome": "refused", "reason": refusal.reason, "layer": refusal.event, "record": refusal.record, "step": refusal.step}
    except Exception:
        result = {"schema": 1, "outcome": "refused", "reason": "policy-error", "layer": "", "record": -1, "step": ""}
    print(json.dumps(result, sort_keys=True))


main()
`

var (
	policyRefusals = []string{"home", "project", "codex", "timeout", "exit", "protocol", "rpc", "set", "managed", "disabled", "policy-error"}
	policySteps    = []string{"", "version", "initialize", "initialized", "config/read", "exit"}
	policyLayers   = []string{"", "mdm", "system", "enterpriseManaged", "user", "project", "legacyManagedConfigTomlFromFile", "legacyManagedConfigTomlFromMdm"}
)

type ShellPolicy struct {
	Legacy  bool
	Exclude []string
}

type ShellPolicyRead struct {
	Codex   string
	Project string
	Exec    func(context.Context, []string) ([]byte, error)
}

type ShellPolicyRefusal struct {
	Reason string
	Layer  string
	Record int
	Step   string
}

func (e ShellPolicyRefusal) Error() string {
	where := ""
	if e.Layer != "" {
		where += fmt.Sprintf(" at the %s layer, record %d", e.Layer, e.Record)
	}
	if e.Step != "" {
		where += " during " + e.Step
	}
	return fmt.Sprintf("the worker's Codex shell environment policy cannot keep the API keys out of tool commands: %s%s", e.Reason, where)
}

func (p ShellPolicy) override() string {
	if !p.Legacy {
		filters := make([]string, 0, len(CredentialEnv()))
		for _, name := range CredentialEnv() {
			filters = append(filters, name+`="exclude"`)
		}
		return "shell_environment_policy.filters={" + strings.Join(filters, ",") + "}"
	}
	patterns := make([]string, 0, len(p.Exclude)+len(CredentialEnv()))
	for _, pattern := range slices.Concat(p.Exclude, CredentialEnv()) {
		patterns = append(patterns, tomlString(pattern))
	}
	return "shell_environment_policy.exclude=[" + strings.Join(patterns, ",") + "]"
}

func tomlString(value string) string {
	raw, _ := json.Marshal(value)
	// TOML basic strings must escape U+007F, which encoding/json leaves raw.
	return strings.ReplaceAll(string(raw), "\x7f", `\u007f`)
}

func (r ShellPolicyRead) Run(ctx context.Context) (ShellPolicy, error) {
	if r.Codex == "" {
		return ShellPolicy{}, errors.New("the inventory pins no codex version, so the worker's shell environment policy cannot be read before its API keys are delivered")
	}
	ctx, cancel := context.WithTimeout(ctx, policyDeadline+grantSlack)
	defer cancel()
	argv := append([]string{"python3", "-c", ShellPolicyScript, r.Codex, r.Project, strconv.Itoa(int(policyDeadline.Seconds()))}, CredentialEnv()...)
	out, err := r.Exec(ctx, argv)
	if err != nil {
		return ShellPolicy{}, fmt.Errorf("read the worker's Codex shell environment policy: %w", err)
	}
	return ParseShellPolicy(out)
}

func ParseShellPolicy(out []byte) (ShellPolicy, error) {
	var result struct {
		Schema  int      `json:"schema"`
		Outcome string   `json:"outcome"`
		Reason  string   `json:"reason"`
		Layer   string   `json:"layer"`
		Record  int      `json:"record"`
		Step    string   `json:"step"`
		Legacy  bool     `json:"legacy"`
		Exclude []string `json:"exclude"`
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.More() {
		return ShellPolicy{}, errors.New("the Codex shell environment policy read printed no single result document")
	}
	if result.Schema != policySchema {
		return ShellPolicy{}, errors.New("the Codex shell environment policy read answered with another schema")
	}
	switch result.Outcome {
	case "refused":
		if !slices.Contains(policyRefusals, result.Reason) || !slices.Contains(policyLayers, result.Layer) || !slices.Contains(policySteps, result.Step) {
			return ShellPolicy{}, errors.New("the Codex shell environment policy read refused for an unrecognized reason")
		}
		return ShellPolicy{}, ShellPolicyRefusal{Reason: result.Reason, Layer: result.Layer, Record: result.Record, Step: result.Step}
	case "exact":
	default:
		return ShellPolicy{}, errors.New("the Codex shell environment policy read reported an unrecognized outcome")
	}
	return ShellPolicy{Legacy: result.Legacy, Exclude: result.Exclude}, nil
}
