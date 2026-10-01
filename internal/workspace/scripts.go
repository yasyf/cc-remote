package workspace

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	warmStamp     = remote.StateDir + "/warm-head"
	bootstrapPath = remote.StateDir + "/bootstrap.sh"
	githubOrigin  = "https://github.com/"
)

func CheckoutScript(root, repository, ref string, shallow bool) string {
	depth := ""
	if shallow {
		depth = " --depth 1"
	}
	return remote.Script(
		"IFS= read -r token || token=''",
		`askpass="$(mktemp)"`,
		`trap 'rm -f "$askpass"' EXIT`,
		`printf '%s\n' '#!/bin/sh' 'case "$1" in *Username*) echo x-access-token ;; *) printf "%s\\n" "$CC_REMOTE_GIT_TOKEN" ;; esac' > "$askpass"`,
		`chmod 700 "$askpass"`,
		`if [ -n "$token" ]; then export CC_REMOTE_GIT_TOKEN="$token" GIT_ASKPASS="$askpass"; fi`,
		"export GIT_TERMINAL_PROMPT=0",
		"root="+remote.Quote(root),
		"ref="+remote.Quote(ref),
		`if [ ! -d "$root/.git" ]; then`,
		`  mkdir -p "$(dirname "$root")"`,
		`  git clone --quiet`+depth+` --branch "$ref" --single-branch `+remote.Quote(repository)+` "$root"`,
		"  exit 0",
		"fi",
		`git -C "$root" fetch --quiet --prune`+depth+` origin "$ref"`,
		`git -C "$root" checkout --quiet --force -B "$ref" FETCH_HEAD`,
		`if git -C "$root" rev-parse --quiet --verify "refs/remotes/origin/$ref" > /dev/null; then git -C "$root" branch --quiet --set-upstream-to "origin/$ref"; fi`,
	)
}

func RefreshScript(root string, env, steps []string) string {
	lines := []string{"cd " + remote.Quote(root)}
	for _, pair := range env {
		lines = append(lines, "export "+remote.Quote(pair))
	}
	return remote.Script(append(lines, steps...)...)
}

func WarmScript(root string, inputs, steps []string) string {
	if len(inputs) == 0 {
		return RefreshScript(root, nil, steps)
	}
	return RefreshScript(root, nil, slices.Concat(
		[]string{
			`if [ -s "` + warmStamp + `" ] && git diff --quiet "$(cat "` + warmStamp + `")" HEAD -- ` + remote.QuoteAll(inputs) + `; then`,
			`echo "` + remote.Prefix + `: nothing the warm steps read changed since $(cat "` + warmStamp + `"); skipping them" >&2`,
			"else",
		},
		steps,
		[]string{`mkdir -p "` + remote.StateDir + `"`, `git rev-parse HEAD > "` + warmStamp + `"`, "fi"},
	))
}

const WriteBootstrapScript = `set -eu
mkdir -p "` + remote.StateDir + `"
cat > "` + bootstrapPath + `"
chmod 700 "` + bootstrapPath + `"`

func BootstrapScript(root string, env []string) string {
	return RefreshScript(root, env, []string{`"` + bootstrapPath + `"`})
}

func ForwardEnv(forwards []Forward, labels []LabelledEnv) []string {
	ports := map[string]int{}
	for _, forward := range forwards {
		ports[forward.Label] = forward.Port
	}
	env := make([]string, 0, len(labels))
	for _, label := range labels {
		env = append(env, fmt.Sprintf("%s=%d", label.Env, ports[label.Label]))
	}
	return env
}

func TokenAllowed(repository string) bool {
	return strings.HasPrefix(repository, githubOrigin)
}
