# Independent agent environments

An Orca workspace can use a small remote environment for editing code, then select
a different profile when its task needs a complete platform. The repository's
setup commands belong to that profile; the environment manager supplies the
host, checkout, tools, and lifecycle.

cc-remote implements this separation with Sprites and Namespace backends,
image and tool preparation, workspace lifecycle, prepared claims, and an Orca
adapter. The [workspace guide](orca-workspaces.md) describes the client setup.
The Orca composer flow and live personal-tailnet enrollment still need live
verification; startup timings for this implementation have not been measured.

## Separate the client from the host

A frontend translates a client's workspace request into a core request. A
backend performs the provider operations. The core coordinates the lifecycle
between them.

```mermaid
flowchart LR
    Orca[Orca frontend] --> Core[cc-remote core]
    Config[Repository and tool configuration] --> Core
    Core --> Sprites[Sprites backend]
    Core --> Namespace[Namespace backend]
    Sprites --> AgentHost[Assigned agent environment]
    Namespace --> AgentHost
```

Orca is the current frontend. Sprites and Namespace are the current backends.
Claude Code and Codex run inside an assigned environment; their choice does not
change the provider lifecycle.

| Layer | Responsibility |
| --- | --- |
| Core | Lifecycle, checkout, tool readiness, prepared capacity, identity, ownership, and spending admission |
| Backend | Provider creation, execution, transport, wake, suspension or idle behavior, and deletion |
| Frontend | Client request mapping, result rendering, workspace registration, and client lifecycle events |
| Configuration | Repository settings, tool inventory, provider choices, and optional setup commands |

Client environment variables and result schemas belong to the frontend. Provider
commands and image choices belong to the backend. The core receives explicit
configuration and has no dependency on a particular repository or its build
infrastructure.

## Prepare tools once

The lean profile selects Sprites over SSH with configured tools and plugins and
a shallow checkout of the requested ref. Project dependency installation and
platform startup are task-specific actions. A profile can supply those commands
when needed, including a full-stack Namespace environment.

Prepared capacity holds unused environments with a verified tool set. A matching
[readiness stamp](tool-inventory.md#fingerprints-and-readiness) allows a claim to
reuse that preparation. The claim still
renews workspace identity and materializes the requested checkout at the pinned
source commit.

| Part of the lean profile | Task-specific setup |
| --- | --- |
| Agent tools and plugins from the inventory | Repository dependency installation |
| Shallow checkout at the requested commit | Code generation and test workloads |
| SSH transport and client connection | Platform services and orchestration |

Installing an agent CLI prepares tools. A profile
that installs the repository's packages performs additional setup, even when the
repository is small. Calling both actions “bootstrap” obscures their different
costs and lifetimes.

## Give each environment its own identity

Images and prepared capacity contain no authenticated agent session, tailnet
state, or retained checkout credential. A claim assigns a distinct host identity
and enrolls the assigned environment on the configured tailnet. Prepared hosts
remain unenrolled.

Cleanup uses the exact provider resource and recorded tailnet node. A retry
distinguishes a node it enrolled from one it reattached. A failed retry preserves
the existing node, and the resource lock covers reattachment, result delivery,
and any enrollment cleanup.

State belongs to `cc-remote`. Explicit state and spending imports preserve
existing ownership and accounting during a cutover; starting with an empty
ledger does not erase previous spending. Agent environments use their own images
and pools, independent of build infrastructure.

## Measure the complete startup path

Provider creation, prepared claiming, SSH readiness, tool readiness, checkout
readiness, and client registration are separate timestamps. A completed test
workload measures useful work after startup.

A prepared claim does not establish the empty-pool create time. An SSH connection
does not establish that the tools and checkout are ready. Keeping those results
separate makes the cost of each layer visible and prevents installation or test
time from being reported as provider startup.
