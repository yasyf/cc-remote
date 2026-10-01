# cc-remote Development Guide

cc-remote is a repo-agnostic Go CLI in early development; only `cc-remote version`
exists. Planned work covers VMs where Claude Code and Codex run against a
repository checkout, with Sprites and Namespace backends and an Orca desktop
frontend. The planned default is lean: agent tools and plugins plus a shallow
checkout of the requested ref, without project dependency installation or Tilt;
a full-stack workspace on Namespace over SSH will require opt-in config.

## Repository Structure

```text
cc-remote/
├── cmd/cc-remote/          # main package
├── internal/
│   ├── cli/               # Cobra command tree; version only
│   ├── version/           # version string, stamped by -ldflags
│   └── log/               # slog setup
├── docs/
│   └── architecture.md    # planned packages
├── .github/workflows/
│   ├── ci.yml             # vet/test -race/build on Ubuntu + macOS; golangci-lint, govulncheck, actionlint
│   ├── release.yml        # tag-driven GitHub releases: linux/darwin amd64/arm64 binaries + SHA256SUMS.txt
│   ├── guides.yml         # render guides from fragments
│   └── cc-notes.yml       # reconcile merged tasks
├── .claude/fragments/     # cc-guides layouts; edit fragments, never rendered files
├── AGENTS.md              # rendered development guide
├── STYLEGUIDE.md          # Go style rules
└── README.md              # project overview
```

CI renders `AGENTS.md`, `CLAUDE.md`, `.gitignore`, `.claude/settings.json`,
`.mcp.json`, and `.pre-commit-config.yaml` from `.claude/fragments/`.

Keep the repo org-agnostic: no credentials, org IDs, image IDs, hostnames, or
private paths. Site values come from a user config file.
