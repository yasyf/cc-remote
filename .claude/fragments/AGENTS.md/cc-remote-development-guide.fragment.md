# cc-remote Development Guide

cc-remote is a repo-agnostic Go CLI for remote agent workspaces, with Sprites and
Namespace backends and an Orca desktop frontend. The default profile prepares
agent tools and plugins plus a shallow checkout of the requested commit.
Project dependency installation and platform startup belong to optional
profile commands. A full platform on Namespace over SSH requires opt-in config.

Read `docs/orca-workspaces.md` for the native composer flow and
`docs/tool-inventory.md` for tool preparation. Recipe generation and waiting
must use the same config and lifecycle binary. Preserve resource ownership
and exclusive workspace claims.

## Repository Structure

```text
cc-remote/
├── cmd/cc-remote/          # main package
├── internal/
│   ├── cli/               # Cobra commands and workspace lifecycle
│   ├── config/            # repository, profiles, and provider configuration
│   ├── frontends/orca/    # recipe generation and client verification
│   ├── images/            # tool inventory and preparation scripts
│   ├── providers/         # Sprites and Namespace backends
│   ├── state/             # workspace records and SSH fragments
│   ├── version/           # version string, stamped by -ldflags
│   └── log/               # slog setup
├── docs/
│   ├── architecture.md    # lifecycle and ownership
│   └── orca-workspaces.md # client setup and commands
├── .github/workflows/
│   ├── ci.yml             # vet/test -race/build on Ubuntu + macOS; golangci-lint, govulncheck, actionlint
│   ├── release.yml        # v* tags call the shared homebrew-tap release-go workflow
│   ├── guides.yml         # render guides from fragments
│   └── cc-notes.yml       # reconcile merged tasks
├── .goreleaser.yaml       # signed linux/darwin binaries and the Homebrew cask
├── .claude/fragments/     # cc-guides layouts; edit fragments, never rendered files
├── AGENTS.md              # rendered development guide
├── STYLEGUIDE.md          # Go style rules
└── README.md              # project overview
```

CI renders `AGENTS.md`, `CLAUDE.md`, `.gitignore`, `.claude/settings.json`,
`.mcp.json`, and `.pre-commit-config.yaml` from `.claude/fragments/`.

Keep the repo org-agnostic: no credentials, org IDs, image IDs, hostnames, or
private paths. Site values come from a user config file.
