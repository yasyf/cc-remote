# Create an Orca workspace

Use cc-remote to prepare a remote machine, then let Orca create and register its
workspace through an environment recipe. The ordinary Orca worktree default
stays unchanged.

The current release and verification status is in the [README](../README.md).

## Configure the repository

Start with [examples/config.yaml](../examples/config.yaml), replace its
placeholders, and save it as `.cc-remote/config.yaml` in the repository. Sign in
to the selected provider's CLI on the machine running cc-remote.

Choose Sprites over SSH for an agent checkout. A profile's `checkout` is
`shallow` by default, a depth-1 clone of the requested ref. `blobless` opts into
a clone with the whole history but no file contents until a checkout or diff
needs them, so a rebase onto an older base finds its merge base; it takes about
twice as long to clone. `full` clones everything. Use a shallow checkout and
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
default. Its `orca.entry` is that tool's `orca` CLI,
`squashfs-root/resources/bin/orca-ide` by default, and must be executable on
the machine. cc-remote starts the runtime as `<entry> serve`, and the guest
bootstrap helper drives the same entry. Orca picks the server behind that
command: from 1.4.224 on Linux it serves on its managed server and falls back
to Electron when the machine cannot run one. The task record's `managedServer`
holds the managed server's build when the ready event reports one and is
absent on Electron. Orca writes its reason for a fallback as a `[serve]` line
in `$HOME/.cc-remote/orca/serve.log` on the machine.

A Claude worker's runtime starts as soon as the machine's tools are installed,
while the create configures the machine, enrolls it in the tailnet, and writes
the ready stamp. A runtime that fails to start fails the command once the
create finishes, and the workspace is kept. A Codex worker's runtime starts
after the create, once cc-remote has read the machine's shell environment
policy.

Before a Codex worker's hooks are trusted, cc-remote compares the machine's
`~/.codex/hooks.json` with one exact set and refuses any other. The set follows
the server in the ready event. On Electron it is Orca's eight handlers plus
Captain Hook's five. Orca's managed server installs no agent hooks, so on a
managed server the set is Captain Hook's five alone, and an Orca handler found
there is refused.

The worker's API key comes from the `orca.keys`
command for its provider; the defaults read the macOS keychain items
`cc-remote-anthropic-api-key` and `cc-remote-openai-api-key`. An optional
`orca.keys.typesafe` command, which has no default, adds a TypeSafe key that
every worker receives as `TYPESAFE_API_KEY`, the variable capt-hook's
`evt.decide` reads on an API actor. Codex excludes it from its shell tools
along with the provider keys. An optional `orca.keys.github` command, which has
no default, adds a GitHub token that every worker receives as `GH_TOKEN`, with
git's credential helper for `https://github.com` set to `gh auth git-credential`
through `GIT_CONFIG_*` variables, so the worker can push its branch and open its
pull request. Codex keeps that token in its shell tools.

An optional
`orca.keys.github_login` command, also with no default, prints the GitHub login
that owns that token, such as `[gh, config, get, -h, github.com, user]`. The
worker's terminal saves it as the `user` of `github.com` in gh's config before
the agent starts, so `gh config get -h github.com user` answers on the machine
without an API call, the same as on the launcher. Only the login is saved,
never the token.

An optional `orca.keys.trunk` command, which has no default, runs locally and adds a scoped Trunk key that every newly launched worker receives as `TRUNK_MERGE_API_KEY`. Like `GH_TOKEN`, it stays available to shell tools rather than being excluded with the model credentials. An optional `orca.keys.gemini` command works the same way, handing every newly launched worker a Gemini key as `GEMINI_API_KEY` so the `gemini` CLI runs from its shell tools.

Every key travels only over SSH stdin into a
one-use pipe that the worker's terminal reads and removes.

A worker commits as the person who launches it. `orca create` and `orca prepare`
read `user.name` and `user.email` from this machine's `git config` and send them
down the same pipe. The worker's terminal sets them through `GIT_CONFIG_*`
variables, which outrank any identity the machine's own git config holds. A
launch refuses before it reaches the provider when either key is unset, and the
refusal names the key. The identity is not written to the task record, the
workspace record, a receipt, a log, or a file on the machine.

Workers start with no Model Context Protocol servers; `--mcp-config` names the servers a
worker may start. A Claude worker starts with `DISABLE_AUTOUPDATER=1`, so its
CLI stays at the version the machine was built with. The frontend accepts Claude and Codex folder trust prompts
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
runtime, the worker, or the forward.

The Orca workspace card's Sleep and Delete controls do not manage the provider
machine; use `cc-remote orca stop`, `cc-remote suspend`, and `cc-remote destroy`.

`cc-remote orca stop lane-name` stops the workspace a lane runs on: the one
`prepare` created under the lane's name, or the warm pool workspace the lane
claimed. It closes the task's SSH forward, removes the Orca environment saved
under the workspace's name, and suspends the machine, then prints the lane and
workspace. The stopped runtime never answers that environment again, and Orca
retries a saved environment that does not answer about every 1.5 seconds for
as long as it stays saved. A Namespace instance stops. Sprites has no stop verb,
so on a Sprite it stops every service and waits up to two minutes for the
Sprite to pause, which it does once no session, request, or connection holds
it. The disk, the workspace record, the task record, and the pool record stay.
A warm claim pairs its own environment, so no stopped workspace needs its old
one back. A warm pool claim's record turns `stopped`, so `prepare --warm`
admits the lane again and claims it a fresh workspace. It refuses a warm pool
workspace's own name, so it stops only a workspace a lane claimed, and it exits nonzero with
the error when the machine does not stop. A claim that failed before its worker
started is not where the lane runs, and neither is a claim already stopped, so
stop passes over both and reaches the workspace the lane moved on to.

A lane with no task record has no forward to close, so stop suspends its
workspace alone. A lane is already stopped when no workspace is recorded for it
or the provider no longer has its machine: stop removes the workspace, SSH,
pool, and task records left for it, prints what it found under `found`, and
exits zero.

A Sprite whose services listing answers 503 `Storage not ready` has lost its
storage at the provider: nothing runs on it again, and its record still reads a
normal cold. stop counts it as stopped, prints the provider's answer under
`found`, and exits zero. It destroys nothing, so the machine and its records stay
for whoever recovers its disk.

A stopped lane's environment can still be saved in Orca, and so can the
environment of a workspace whose machine was deleted some other way. Orca
rereads every saved environment on each lookup, so each one it no longer needs
costs it work. `cc-remote orca prune` removes the saved environment of every
task on the selected provider whose lane was stopped or whose machine the
provider no longer has, and prints each one it removed with the reason.
`--dry-run` prints the same list and removes nothing.

prune matches an environment
by the id the task recorded when it paired, so a later pairing under the same
name stays, and it never touches an environment no task paired. It also
releases a task whose machine is gone, so keep stops reopening that forward.
The records stay.

`reconnect` restores only the transport to an existing runtime. It does not
resume a machine, restart a runtime, or recover a worker after process loss.
The recorded runtime and receipt identities remain unchanged.

The machine itself restarts a runtime that exits. Its launcher,
`$HOME/.cc-remote/orca/serve.sh`, runs the runtime in a loop and waits before
each restart: 1 second at first, twice as long after each exit up to 30
seconds, and 1 second again after a run of a minute or more. The agents in the
worker's terminals keep running across the restart. Only the launcher holds
`serve.lock`; the runtime and the processes it starts run without it. Before
each restart the launcher writes `cc-remote: the Orca runtime exited <status>;
starting it again in <delay>s` to its stderr and to `serve.log`. When that run
logged `ENOSPC`, or the disk under the home has less than 64 MiB free, it first
writes a line naming that disk's mount point, its device and the MiB it has
free. A restarted runtime answers under a new runtime id, so `reconnect` and
`status`, which ask for the recorded one, fail for that task.

An SSH forward can exit on its own while its worker keeps running, for example
after 45 seconds without an answer from the Sprite. Once `create`, `prepare`, or
`reconnect` has a worker answering over an SSH forward, it keeps the task and
starts `cc-remote orca keep` in the background. One keep runs per state
directory, so a Mac runs one however many lanes it holds. Every 30 seconds it
reopens each kept forward that has exited, the way `reconnect` does, and it
exits once no task is kept. Each reopen gets one minute, so a machine that
answers nothing costs a pass that minute and never holds up the forwards after
it. When a reopen fails, keep asks the provider whether the machine's storage is
reachable. A machine that still reports its storage gone 10 minutes after its
first failed reopen runs nothing again, so keep releases its task, logs that
once, and stops reopening it; `reconnect` keeps it again. The 10 minutes pass
over a platform restart, during which a live machine answers `Storage not
ready` for a few seconds. `orca stop` and `destroy` release the task before
they close its forward, so keep never reopens a stopped lane's forward. A task
launched by a release without keep is not kept until `reconnect` runs on it.
keep logs to `orca/keeper.log` in the state directory.

keep also runs `orca prune` on its first pass and every 10 minutes after. A
failed prune is logged and tried again on the next one, and it never stops keep.

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

`--before-worker-file` names a script that `prepare` runs with `sh` on the
machine after the checkout and before the worker starts, so the first session
sees whatever it installs. A nonzero exit stops `prepare` with no worker. The
task record carries the script's stdout as `beforeWorker`.

Neither command initializes WordNet. The brief of a task that needs it in place
tells the worker to
[provision it first](tool-inventory.md#provision-wordnet-for-a-task-that-needs-it).

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
steps and a `shallow` or `blobless` checkout. The command refuses any other
provider or profile before it changes anything. `--warm` and `--existing` exclude each
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

A Claude worker's runtime starts once the tools pass that check, while the
fetch and configure run. A Codex worker's runtime starts after its hook grant
is revalidated.

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

`--warm` checks `orca.bootstrap_helper` against the running release before it
claims, so a CLI the helper pin does not name is refused with the pool
untouched. A claimed workspace never returns to the pool. A failed preparation is
recorded `failed` with its error's metadata only: the step that failed, the
error's class, the exit code when one was observed, the HTTP status when a
provider lookup answered one, and the message's byte length and SHA-256, never
its text. When that happens before the worker's terminal starts, the command
logs a warning and stops that workspace's machine. It first checks that no
agent process runs there, then removes the workspace's Orca environment,
suspends the machine as `cc-remote orca stop` does, and records the claim
`stopped`, keeping its disk and records. A machine where an agent runs, or one
that does not answer the check, stays running and `failed`. So does one the
claim never confirmed unused, such as a checkout that cannot be read or a
machine that already holds Orca runtime state, because the claim did not make
what runs there. The command then
claims the next unused workspace, and creates the lane's own workspace once
none is left. A lane stops after two failed claims and reports
both, so a preparation that fails on every workspace cannot consume the pool. A
failure after the terminal starts ends the command. A command that
dies leaves its workspace `claimed`.
A worker that starts is recorded `prepared` with its HEAD. Each lane gets one
first worker, so `--warm` refuses a lane that already has an Orca task, already
claimed a workspace its worker started on, or names a pool workspace. Two
claims do not count, and a relaunch of the lane claims the next unused
workspace past them: one that failed before its worker's terminal started, and
one `cc-remote orca stop` stopped. A launcher whose own steps fail after
`prepare` and before it hands the worker its task stops the lane, so the
relaunch is admitted and the idle worker does not hold its machine awake.

The lane's own workspace, made when the pool was empty, follows the same rule.
Its task record turns `stopped` when `cc-remote orca stop` stops it, and a
relaunch is admitted once that record is stopped or never recorded a terminal.
The relaunch then claims a warm workspace. The first worker on a machine never
adopts the runtime state an earlier one left, so the relaunch does not reuse
the lane's own workspace. With no warm workspace ready it is refused, because
it cannot create a second workspace under the lane's name; wait for a pool
fill to ready one, then launch again.

A ready workspace the provider cannot answer for while it is being claimed,
such as a Sprites 503, stays ready, and the claim moves to the next one. The
claim fails with each unanswered lookup only when none of the ready workspaces
could be observed.

#### Keep the pool filled

`orca.pool.ready` sets how many unused workspaces to keep, `1` by default. It's
a readiness setting, not a spending cap. Set it to `0` to stop the background
fill; `--warm` then claims only workspaces that already exist.

Once it has claimed, or found the pool empty, `prepare --warm` starts one
`cc-remote orca pool fill` in a session of its own, with stdin on `/dev/null`
and stdout and stderr appended to `pool/fill-<key>.log` under the state
directory. It doesn't wait for the fill, and the fill can't hold its stdout
open. A fill that fails to start shows up as `replenish.failure`, and
the worker is still prepared.

Run the fill yourself to top up the pool, then list it:

```sh
cc-remote orca pool fill --config .cc-remote/config.yaml
cc-remote orca pool status --config .cc-remote/config.yaml
```

Fills of the same key run side by side, and each stops at a 20-minute
deadline. Under the pool's state lock a fill marks a `provisioning` workspace
whose fill died as `abandoned`, counts one whose fill is still creating it,
refuses a `ready` one that no longer passes the claim checks, and reserves
enough new workspaces to bring the ready and in-flight count to
`orca.pool.ready`. It then creates those workspaces at once, so a claim that
leaves the pool one short makes exactly one create, and a burst of claims
refills in about one create's time instead of one after another. Each is a
plain `create` with a `pool-` name and the pool label, so it holds tools,
plugins, and a shallow checkout of `config.ref`. A fill starts no Orca runtime,
records no task, starts no worker, and reads no API key. A failed create is
recorded `failed` with the same error metadata and logged as a warning, and the
fill reports it; the other creates go on, and the next claim's fill counts the
shortfall again and replaces it.

The record's `step` names where the create failed. Once the Sprite exists it
is the create phase, such as `payload.mount`, `tools.install`, or `checkout`,
and `exit` holds the exit code of the command that phase ran on the machine.
`class` says what kind of error ended it: `exit` for a command that exited
non-zero, `http` for a provider lookup that answered an unexpected status,
`deadline` for the fill's deadline, `canceled` for a caller that stopped it, and
`other` for anything else.

When the Sprite create itself fails, `step` names which part of it
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

`cc-remote destroy <name>` removes the name's pool record with its workspace
record. When the name has a task record, destroy first closes its SSH forward
and removes its Orca environment, as `orca stop` does. When a name has a pool record but no workspace record, destroy removes
the pool record, logs that it touched no machine, and exits zero, so a record
whose machine is long gone can be cleared by name. `status` and `fill` never
remove a record on their own.

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
