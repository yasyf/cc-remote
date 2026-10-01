# Planned architecture

cc-remote will create and manage remote agent workspaces on Sprites and Namespace,
with Orca as a planned desktop frontend. It will own images, prepared pools, state,
identity, tailnet enrollment, and tooling installation. Today, only the CLI
scaffold exists: `cmd/cc-remote`, `internal/cli`, `internal/version`, and
`internal/log`; the only command is `cc-remote version`.

## Planned packages

These Go package paths are placeholders; none exists yet.

- `internal/lifecycle` will manage core workspace lifecycle and state.
- `internal/providers/sprites` will implement the Sprites backend.
- `internal/providers/namespace` will implement the Namespace backend.
- `internal/pools` will manage prepared workspace pools.
- `internal/identity` will manage workspace identity.
- `internal/tailnet` will enroll workspaces in a tailnet.
- `internal/images` will own workspace images and install agent tools and plugins.
- `internal/frontends/orca` will connect the planned Orca desktop frontend to the workspace lifecycle.
- `internal/config` will read user config for site values and opt-in full-stack workspaces on Namespace over SSH.
