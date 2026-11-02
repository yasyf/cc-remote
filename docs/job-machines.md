# Keep job machines ready

Heavy one-off work, such as a build, a rebase, or a test pack, runs on a job machine instead of your own. `cc-remote jobs` keeps a pool of them. A released job machine returns to the pool for the next claim.

```yaml
jobs:
  ready: 4
  prefix: job
  provision: [./provision-job.sh]
  reset: [sh, -c, "git -C ~/repo worktree prune"]
```

`keep` provisions machines named `job-<id>` until `ready` of them are idle. It runs `provision` from the config file's directory with the new name as the last argument. `adopt` records machines you provisioned another way, and returns a failed machine you repaired to ready. Run `keep` on a timer.

```sh
cc-remote jobs keep
cc-remote jobs adopt build-1 build-2
```

`run` claims a machine for one command. It sets `SPRITE` to the machine's name, exits with the command's status, and releases the machine however the command ends.

```sh
cc-remote jobs run --holder restack -- sh -c 'sprite exec -s "$SPRITE" -- make test'
```

To hold a machine across several commands, use `claim` and `release`.

```sh
machine=$(cc-remote jobs claim --holder build-lane)
cc-remote jobs release "$machine"
```

Each claim stores a fresh token from `git.token_command` in the machine's git credential cache. The token travels over standard input, never in a file or an argument. `keep` refreshes the token on every machine held for 45 minutes or more. A machine whose credential or reset fails is marked failed and leaves the pool, and the next `keep` replaces it. `cc-remote jobs status` prints every record.
