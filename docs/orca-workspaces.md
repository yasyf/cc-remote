# Create an Orca workspace

Use cc-remote to prepare a remote machine, then let Orca create and register its
workspace through an environment recipe. The ordinary Orca worktree default
stays unchanged.

The current release and verification status is in the [README](../README.md).

## Configure the repository

Start with [examples/config.yaml](../examples/config.yaml), replace its
placeholders, and save it as `.cc-remote/config.yaml` in the repository. Sign in
to the selected provider's CLI on the machine running cc-remote.

Choose Sprites over SSH for an agent checkout. Use a shallow checkout and
prepare the agent tools and plugins, leaving project dependency installation
and platform services out of the default profile. Choose a separate Namespace
profile when the task needs the full platform. The repository's configuration
supplies those project commands.

Save the [tool inventory](tool-inventory.md) beside the config as
`.cc-remote/inventory.yaml`, replacing its placeholders. The config's
`inventory` path resolves relative to the config file. All workspace profiles
use the inventory's base tools; a matching inventory profile adds its optional
tools and preparation. The config's forwarding variables must match the
inventory's `configure.env` exactly.

## Check the configuration

Verify the configuration and provider access before creating a workspace:

```sh
cc-remote verify --config .cc-remote/config.yaml
```

`verify` checks configuration, state, provider authentication, Git token
availability, and the tailnet client when configured. It does not create a
machine or authenticate that token against the repository.

## Generate and select a recipe

From the repository's primary checkout, write the environment recipes into
`orca.yaml`:

```sh
cc-remote orca recipes --config .cc-remote/config.yaml --orca-yaml orca.yaml
```

Orca runs these commands from the repository root in a non-login shell. If
`cc-remote` is unavailable on its `PATH`, use `--binary` with a repository
wrapper, such as `--binary ./tools/cc-remote/bin/cc-remote`, when generating
recipes. Pass that same value to `wait`.

The `--plugin` option packages the same recipes as an Orca plugin in an output
directory. The `wait` workflow below requires the generated `orca.yaml`;
its preflight does not inspect installed plugins. For a plugin-only setup,
create the workspace in the composer and run `verify` after registration.

Recipe IDs contain the provider, configured profile name, and `ssh`. The
configured default has `(default)` in its display name. That label does not
change Orca's worktree settings.

With the example's `lean` profile, start the preflight and wait:

```sh
cc-remote orca wait sprites-lean-ssh task-name --config .cc-remote/config.yaml
```

The preflight also requires an `Include` line in `~/.ssh/config` for the SSH
fragments under the configured state directory. If it is missing, add the exact
line reported by `wait` above every `Host` and `Match` block, then retry.

In Orca's New Workspace composer, choose this repository, use `task-name` as the
workspace name, and select the recipe under **Run on**. Orca runs the recipe
and registers the SSH workspace returned by cc-remote. `wait` observes that
registration; it does not create the workspace itself.

## Check the client connection

After registration, check the workspace from the Orca client:

```sh
cc-remote orca verify task-name
```

This opens a file in the editor and runs a probe in a new remote terminal. The
probe requires `orca` to resolve to `$HOME/.orca-relay/bin/orca` and complete
`orca worktree current --json` through the attached SSH relay before reporting
the remote hostname, Git branch, and directory. The probe terminal stays open,
and the command returns its handle.

The [remote-workspaces skill](../skills/remote-workspaces/SKILL.md) covers recipe
selection, preparation, and workspace lifecycle. Initial cc-remote recipes use
SSH. Orca-server transport requires a separate implementation.

## Start a worker from the Orca CLI

`cc-remote orca create` runs a task without the composer. The command creates
a workspace on a fresh machine and starts the Orca runtime there as a provider
service. An SSH forward carries the runtime's loopback port to this machine,
whose Orca CLI pairs with it as an environment named after the workspace. The
workspace's existing checkout becomes an Orca repo, and the worker starts in an
Orca terminal on that checkout.

```sh
cc-remote orca create task-name --config .cc-remote/config.yaml \
  --agent claude --model <model> --effort <effort> --prompt-file prompt.md
```

The runtime is the inventory tool named by `orca.tool`, `orca-runtime` by
default, and its `orca.entry`, `squashfs-root/AppRun` by default, must be
executable on the machine. The worker's API key comes from the `orca.keys`
command for its provider; the defaults read the macOS keychain items
`cc-remote-anthropic-api-key` and `cc-remote-openai-api-key`. The key travels
only over SSH stdin into a one-use pipe that the worker's terminal reads and
removes. Workers start with no Model Context Protocol servers; `--mcp-config` names the servers a
worker may start. `create` accepts Claude's folder trust prompt only when
`orca.trust` lists the repository's owner.

```yaml
orca:
  trust: [<org>]
```

| Command | Result |
| --- | --- |
| `cc-remote orca status task-name` | Reports the forward and whether the environment answers from the recorded runtime. |
| `cc-remote orca reconnect task-name` | Reopens the SSH forward and verifies the recorded live runtime. |
| `cc-remote orca send task-name --prompt-file next.md` | Sends a prompt and prints its receipt. |
| `cc-remote orca read task-name` | Prints the worker terminal's rendered screen. |

A receipt counts as submitted only when Orca reports `turn_started`. `send`
keeps accepted receipts when no turn start was observed. Use `status` and `read`
to inspect delivery before deciding what to send next. The frontend does not
expose native request replay because the supported runtime can deliver the
prompt again. These commands never stop the
runtime, the worker, or the forward. The Orca workspace card's Sleep and Delete
controls do not manage the provider machine; use `cc-remote suspend` and
`cc-remote destroy`.

`reconnect` restores only the transport to an existing runtime. It does not
resume a machine, restart a runtime, or recover a worker after process loss.
The recorded runtime and receipt identities remain unchanged.
