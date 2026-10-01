---
name: remote-workspaces
description: Create, check, and retire a remote Orca workspace that cc-remote provisions on Sprites or Namespace. Use when an agent or task needs its own remote machine, when asked to pick a "Run on" recipe, to wait for or verify a remote workspace in Orca, to explain what SSH transport gives an Orca workspace, or to sleep, wake, or delete a remote workspace.
---

# Manage remote workspaces

cc-remote provisions a remote machine for one Orca workspace, and Orca owns
that workspace. Orca runs the recipe's `create`, registers the SSH target the
recipe returns, and later runs `suspend`, `resume`, and `destroy` from the
workspace card.

Every `orca` command in this skill means the Orca CLI resolved the way cc-remote
resolves it: the value of `ORCA_CLI_COMMAND` when set, `orca-dev` in an Orca
dev checkout that exposes `ORCA_DEV_REPO_ROOT`, `orca-ide` on Linux outside an
Orca terminal, and `orca` otherwise. On Linux outside Orca, bare `orca` is
usually the GNOME desktop screen reader at `/usr/bin/orca`.

The composer flow below follows Orca 1.4.215's CLI help and bundled
`orca-per-workspace-env` guide. Nobody has run it end to end through the Orca
UI with cc-remote yet. Report a label, menu item, or result that differs as a
finding; do not work around it.

## Pick a recipe

Recipe ids follow `<provider>-<profile>-ssh`. cc-remote derives one recipe for
each provider a profile's `machine` section names, so the profile names in the
config become the ids. A profile with no `machine` entries gets one recipe, on
the config's top-level `provider`. Generation fails when a named machine cannot
run, such as a Namespace machine with no image or size. The config's top-level
`provider` and `profile` make the default recipe, and its name ends in
`(default)`. Print the generated set:

```sh
cc-remote orca recipes
```

- Pick the `(default)` recipe for agents and tests.
- Pick another recipe only when the task needs what its profile prepares, such
  as a running project stack on a larger Namespace devbox.

Orca has no default-recipe setting. `(default)` in a recipe name only labels
the row.

## Know what SSH transport gives you

Every cc-remote recipe connects over SSH. The Orca app and runtime on your Mac
stay in charge, and the editor, diffs, git, terminals, and agent sessions work
against the remote checkout over that SSH connection, with Orca forwarding
ports over it. This is the full Orca workspace, not a remote terminal. Orca
lists the machine as an SSH target, selected with `--host ssh:<id>`.

Orca's other transport runs a headless Orca runtime, `orca serve`, on the
machine and pairs your client with it as a server connection
(`--environment <name>`). cc-remote does not implement that transport.

The recipes set `checkoutMode: provisioned-root`. Orca passes the pinned
source to `create` in `ORCA_REPO_URL`, `ORCA_REPO_REF`, `ORCA_REPO_REF_HEAD`,
and `ORCA_REPO_BRANCH`, and uses the checkout `create` returns as the
workspace root instead of making its own worktree.

## Set up the recipes once per repo

Write the recipes into the repo's `orca.yaml` and commit it on the primary
branch; the composer reads `environmentRecipes` from the primary checkout.

```sh
cc-remote orca recipes --orca-yaml orca.yaml
```

The command rewrites only the `environmentRecipes` key. Rerun it whenever the
cc-remote config changes. `wait` refuses a recipe whose `orca.yaml` entry
differs from what cc-remote generates.

`recipes` and `wait` read the config at `$CC_REMOTE_CONFIG`, or else at
`$XDG_CONFIG_HOME/cc-remote/config.yaml`, where `$XDG_CONFIG_HOME` defaults to
`~/.config`. The generated commands carry that file's absolute path, because
Orca does not run them with your shell's environment. To commit an `orca.yaml`
that works on other machines, pass `--config` with a config checked into the
repo, named relative to the repo root; the commands carry that path unchanged.
Pass the same `--config` to `wait`.

Orca runs recipe commands from the repo root in a non-login shell, so a bare
`cc-remote` must be on the `PATH` Orca starts with. To run a wrapper checked
into the repo instead, pass `--binary` with a `./`-prefixed path, such as
`--binary ./tools/cc-remote/bin/cc-remote`. The recipes carry that path
unchanged, and `wait` needs the same `--binary`.

cc-remote writes each workspace's SSH settings as a `Host` block in
`<state>/ssh/<workspace>.ssh`. `<state>` is the config's `state_dir`, or
`$XDG_STATE_HOME/cc-remote` when it is unset, where `$XDG_STATE_HOME` defaults
to `~/.local/state`. Orca resolves a workspace's host through your SSH config,
so add an `Include` of those blocks above every `Host` and `Match` block in
`~/.ssh/config`. With the default state directory, the line is:

```sshconfig
Include ~/.local/state/cc-remote/ssh/*.ssh
```

`wait` refuses to start until that `Include` is there, and prints the line for
your state directory when it is missing.

`cc-remote orca recipes --plugin <dir>` writes the same recipes as an Orca
plugin, an `orca-plugin.json` that lists them under `contributes.vmRecipes`.
Prefer `orca.yaml`: `orca vm recipe doctor` reads only `orca.yaml`, so `wait`
cannot check a plugin-only recipe.

## Create a workspace

Create a remote workspace only through the Orca desktop `New Workspace`
composer. The `orca` CLI cannot create one: `orca worktree create` has no
recipe option, and no command adds an SSH host. Orca registers the SSH target
itself from the recipe's `create` result.

1. Start the wait in the background or in another terminal. It blocks until
   Orca lists the workspace:

   ```sh
   cc-remote orca wait <recipe-id> <workspace-name>
   ```

   Before it waits, `wait` checks that the Orca runtime is ready with a
   window, that `orca.yaml` carries the recipe exactly as cc-remote generates
   it, and that `orca vm recipe doctor` reports no failure. Fix any failure
   before creating anything.
2. Open the composer with `Cmd+N`.
3. Set `Project` to the repo and `Workspace name` to the name you passed to
   `wait`.
4. In `Run on`, pick the recipe's row under `Per-Workspace Environment`.
5. Leave `Agent` empty and click `Create` once. A second click provisions a
   second machine.

Once Orca lists an SSH workspace with that name, `wait` prints one JSON
object with `recipe`, `name`, `checkout`, `workspace`, `worktree`, `host`,
`path`, and `branch`. Record `host` for the delete check. `--timeout` sets how
long it waits, 30 minutes by default.

The owner operates the composer, or the agent does through `orca computer`
when `orca computer get-app-state --app com.stablyai.orca` can read Orca. If
that read fails with `permission_denied` while `orca computer permissions`
reports access granted, stop and ask the owner to turn Orca's Accessibility
permission off and on. Never change that setting yourself or restart Orca.

Never create a remote workspace any other way. Running a recipe's `create`
command yourself skips Orca's runtime record, so Orca never suspends or
destroys that machine. Never call Orca's renderer interprocess communication
handlers or `orca eval`.

## Verify the workspace from the Orca client

```sh
cc-remote orca verify <workspace-name>
```

`verify` checks that the SSH target is connected, opens `README.md` in the
Orca editor, and runs a probe in a new `remote-check` terminal on the
workspace. `--open` selects another file. It passes only when the probe exits
`0` on the remote host, not on your Mac. On success it prints one JSON object
with `worktree`, `host`, `target`, `connected`, `hostname`, `branch`, `cwd`,
`editor`, and `probeTerminal`.

The probe terminal stays open and belongs to the task. Never close it, or any
other terminal or agent session, as cleanup.

Start an agent in the workspace with a terminal on its worktree id. A Codex
command must pass `--dangerously-bypass-approvals-and-sandbox`:

```sh
orca terminal create --worktree id:<worktree> --command 'codex --dangerously-bypass-approvals-and-sandbox'
```

## Sleep, wake, or delete a workspace

Use the workspace card for every lifecycle action. Before each `Sleep` or
`Delete Worktree`, get the owner's explicit authorization for that exact
workspace. Never sleep or delete a workspace as implicit cleanup when a task
finishes, or while a live session the owner has not released runs there.
Leave a finished task's workspace running.

| Action | On the card | Owner authorization | Orca then |
| --- | --- | --- | --- |
| Sleep | Right-click, choose `Sleep` | Required | Stops the workspace's terminals and agent sessions and runs the recipe's `suspend` |
| Wake | Select the card | Not needed | Runs `resume` and reconnects; rerun `verify` afterwards |
| Delete | Right-click, choose `Delete Worktree`, confirm | Required | Runs `destroy` and removes the SSH target |

After an authorized delete, check that Orca kept nothing on that host:

```sh
cc-remote orca gone <host>
```

`orca worktree rm` and `orca terminal close --all` skip the recipe lifecycle.
Never use them on a remote workspace.

## Record timings

Record your own measurements here; they are observations, not promises.

| Recipe | Fresh create to SSH-ready (p50/p90) | Resume | Source |
| --- | --- | --- | --- |
| `<default recipe>` | `<fill>` | `<fill>` | `<link to your run log>` |

Set `wait --timeout` above the p90 of your fresh creates.
