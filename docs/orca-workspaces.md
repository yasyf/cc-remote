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
worker may start. The frontend accepts Claude and Codex folder trust prompts
only when `orca.trust` lists the repository's owner.

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
controls do not manage the provider machine; use `cc-remote suspend` on a
Namespace workspace and `cc-remote destroy`. `cc-remote suspend` fails on a
Sprite and changes nothing: Sprites has no stop verb, and the open forward
keeps the Sprite active, so only `cc-remote destroy` frees it.

`reconnect` restores only the transport to an existing runtime. It does not
resume a machine, restart a runtime, or recover a worker after process loss.
The recorded runtime and receipt identities remain unchanged.

## Prepare a worker for a Mac Run

`cc-remote orca prepare` runs the same steps as `create` and sends no prompt.
It stops once the worker is idle and copies `--brief-file` byte for byte to
`$HOME/.cc-remote/orca/tasks/task-name/brief.md` on the machine, outside the
checkout, then checks the copy's SHA-256 and length. The task record then
carries `prepared: true`, the brief's path, hash, and size, and the checkout's
HEAD as `baseCommit`. The first task reaches the worker through Orca's
`worker-start --on --terminal --worktree` from the Mac Run, and later messages
go to its home Dispatch, so `send` refuses a prepared task.

```sh
cc-remote orca prepare task-name --config .cc-remote/config.yaml \
  --agent codex --model <model> --effort <effort> --service-tier fast \
  --brief-file brief.md
```

`prepare` takes `--mcp-config` only inline, as a JSON object for Claude or a
TOML table for Codex, because a path names a file on this machine. It
refuses Fable models, which run only as an explicit local choice.
`--service-tier` is a Codex setting on both `prepare` and `create`.

### Prepare the first worker on a recorded workspace

`prepare --existing` starts the first worker of a workspace that
`cc-remote create` already made, instead of creating a machine. Pass the
workspace's recorded provider and profile:

```sh
cc-remote orca prepare task-name --existing --config .cc-remote/config.yaml \
  --provider sprites --profile lean \
  --agent claude --model <model> --effort <effort> --brief-file brief.md
```

The command resumes the exact recorded machine as `cc-remote resume` does and
keeps its checkout as it is. It never clones, fetches, switches branches,
resets, or cleans, and it records the checkout's actual HEAD as `baseCommit`.
It takes no `--ref`. Before it reads the API key or starts the runtime, it
refuses when:

- the workspace already has an Orca task record, complete or partial
- the recorded machine is missing or lacks the workspace's ownership label,
  as a same-name replacement does
- the configured project root is not the checkout's top level, or its origin
  is not the configured repository
- `$HOME/.cc-remote/orca` already exists on the machine

The key, worker, brief, and task record then follow the ordinary `prepare`
steps. A failure keeps the machine, any saved task record, and any started
worker or forward as they are.

Resume reinstalls tools whose stamp no longer matches the configuration. After
such a reinstall the workspace's tools were not all preinstalled, so a
measurement that assumes they were does not hold.

### Prepare a worker from the warm pool

`prepare --warm` keeps unused agent-only Sprites ready and starts each worker
on one of them, so a lane skips machine creation and tool installation. The
name is the lane the worker is for, not a machine name:

```sh
cc-remote orca prepare lane-name --warm --config .cc-remote/config.yaml \
  --ref main --agent claude --model <model> --effort <effort> \
  --brief-file brief.md
```

The pool needs the `sprites` provider and an agent-only profile: no `prepare`
steps and a shallow checkout. The command refuses any other provider or
profile before it changes anything. `--warm` and `--existing` exclude each
other, and `--existing` keeps its retained checkout and its refusal of `--ref`.

The command claims the oldest unused workspace in the pool. A candidate has to
pass checks that read state without touching its machine:

- its record under `<state_dir>/pool/` says `ready` and carries the current
  compatibility key, a hash of the repository, provider, profile, project root,
  checkout mode, rendered tools stamp, image, tailnet tag, and prepare steps
- no other command holds its task lock, and no Orca task is recorded for it
- its workspace record names a verified machine, and the provider reports that
  machine with both `cc-remote/workspace=<name>` and `cc-remote/pool=<key>`

A candidate that fails a check becomes `refused` and is never offered again. A
changed config or inventory changes the key, so older workspaces stay unused
instead of matching. If the provider can't answer, the command fails without
claiming anything. It reads only the pool's own records and never lists,
adopts, renames, stops, or deletes other Sprites.

The claim is recorded with the lane and ref before anything touches the
machine. The command then checks that the checkout is the configured
repository's clean top level with no Orca runtime state, wakes the Sprite, and
requires its tools to be ready at the current stamp; it never reinstalls them.
It fetches `--ref` with `--depth 1` and checks out the fetched commit, so a
branch that moved since the workspace was made is honored, not matched by
name. After configure, the key, worker, and brief follow the ordinary `prepare`
steps. Nothing installs project dependencies or runs profile commands.

If no candidate passes, the command creates the lane's own workspace exactly as
`prepare lane-name` does, and reports `created` instead of `warm`.

stdout carries one object: the lane, the actual workspace, how it was
allocated, the HEAD the task recorded as `baseCommit`, the fill it started, and
the full task record, shortened here:

```json
{
  "schemaVersion": 1,
  "lane": "lane-name",
  "workspace": "pool-<id>",
  "allocation": "warm",
  "ref": "main",
  "head": "<full commit>",
  "replenish": {"target": 1, "pid": 4242},
  "task": {"workspace": "pool-<id>", "prepared": true, "baseCommit": "<full commit>"}
}
```

The task record, task lock, Orca environment, terminal title, and
`$HOME/.cc-remote/orca/tasks/<workspace>/` take the workspace's name, never the
lane's. Pass `workspace` to `status`, `read`, and `collect`.

A claimed workspace never returns to the pool. A failed preparation is
recorded `failed` with its error's metadata only: the exit code when one was
observed, the HTTP status when a provider lookup answered one, and the message's
byte length and SHA-256, never its text. The command neither tries another workspace nor
creates one. A command that dies leaves its workspace `claimed`.
A worker that starts is recorded `prepared` with its HEAD. Each lane gets one
first worker, so `--warm` refuses a lane that already has an Orca task, already
claimed a workspace, or names a pool workspace.

#### Keep the pool filled

`orca.pool.ready` sets how many unused workspaces to keep, `1` by default. It's
a readiness setting, not a spending cap. Set it to `0` to stop the background
fill; `--warm` then claims only workspaces that already exist.

Once it has claimed, or found the pool empty, `prepare --warm` starts one
`cc-remote orca pool fill` in a session of its own, with its standard streams
on `/dev/null`. It doesn't wait for the fill, and the fill can't hold its
stdout open. A fill that fails to start shows up as `replenish.failure`, and
the worker is still prepared.

Run the fill yourself to top up the pool, then list it:

```sh
cc-remote orca pool fill --config .cc-remote/config.yaml
cc-remote orca pool status --config .cc-remote/config.yaml
```

`fill` exits at once when another fill of the same key is running, since that
fill counts again before it ends, and it stops at a 20-minute deadline. It
marks a `provisioning` workspace left by a fill that died as `abandoned`,
refuses a `ready` one that no longer passes the claim checks, and creates
workspaces until `orca.pool.ready` eligible ones are `ready`. Each is a plain `create` with a `pool-`
name and the pool label, so it holds tools, plugins, and a shallow checkout of
`config.ref`. A fill starts no Orca runtime, records no task, starts no worker,
and reads no API key. A failed create is recorded `failed` with the same error metadata and ends the
fill.

When the Sprite create itself fails, the record's `step` names which part
failed. `create.validate` is the local name and spec check, `create.preflight`
the lookup before the create, `create.command` the `sprite create` call,
`create.readback` the lookup after it, and `create.record` the saved labels.
`httpStatus` is set only when one of those lookups answered with an unexpected
status. `sprite create` reports no structured error, so a failed
`create.command` keeps only its exit code. The fill makes no retry and destroys
nothing. The workspace record stays unverified, except after a preflight that
found the name already taken, which removes it.

`status` prints the key, the target, and every pool record with its state:
`provisioning`, `ready`, `claimed`, `prepared`, `failed`, `abandoned`, or
`refused`.

`orca create`, `orca prepare`, and `create` or `resume` with
`--connection server` each hold the workspace's task lock,
`<state_dir>/orca/<name>.json.lock`, from before their first effect until they
return. `prepare --warm` holds the lane's lock and, once it claims, the claimed
workspace's lock too. A second of these commands for the same name refuses at
once without changing anything. `orca create`, `orca prepare`, and `create --connection
server` also refuse a name that already has an Orca task record, including one
left behind by a destroyed workspace; `resume --connection server` instead
requires the saved task.

The worker writes its report and a full binary patch beside its brief.
`collect` copies both into a new local directory:

```sh
cc-remote orca collect task-name --config .cc-remote/config.yaml \
  --report-file /home/<user>/.cc-remote/orca/tasks/task-name/report.json \
  --patch-file /home/<user>/.cc-remote/orca/tasks/task-name/change.patch \
  --output ./task-name-result
```

The report names the base, the patch, and every changed path:

```json
{
  "schemaVersion": 1,
  "baseCommit": "<full commit>",
  "patch": {"sha256": "<hex>", "bytes": 1234},
  "files": [
    {"path": "src/app.go", "sha256": "<hex>"},
    {"path": "old.go", "deleted": true}
  ]
}
```

`collect` requires the recorded live runtime and a HEAD still at `baseCommit`.
It refuses links and files outside the brief's directory, checks the patch
against the report, and prints both files' hashes with up to 65,536 bytes of
`git status` entries, setting `overflow` when more remain. It changes nothing
on the machine. A matching report does not prove that the patch holds every
change.
