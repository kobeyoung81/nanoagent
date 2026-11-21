# nanoagent

The thing that actually runs the turn. One container per person: the model loop,
the tools, and the workspace the agent lives in.

It is an **execution unit, not a first-class service** — it has no database of its
own, and it is created on demand by the provisioner.

## What it is

- **The ReAct loop.** Prompt, model, tool calls, observation, answer — with provider
  failover and a context budget it manages itself.
- **Real tools.** Shell, file read and write, glob, grep, a git panel, web fetch and
  search, cron, background sub-agents and MCP servers.
- **Skills and prompts.** Bundled ones seeded from the platform catalogue, plus
  per-agent ones the person writes.
- **A workspace that lasts.** Files, memory and the conversation transcript live in
  the container's volume; the transcript is the source of truth.
- **A hypervisor.** The small HTTP bridge the platform dials to reach those files,
  the terminal and the git service.

## Where it sits

| | |
|---|---|
| Collections | none — the agent holds no database connection |
| Storage | its own volume: workspace files, `memory/`, and one JSONL transcript per session |
| Image | `docker.io/kobeyoung81/nanoagent:<version>`; the binary inside is `nanosec` |
| Talks to | `nanobus` (messages), `nanoportal` (its own row and settings), `nanoadmin` (configuration and the catalogue) |
| Started by | `nanoprovisioner`, one container per user, with the tier's limits |

## Quick start

The image is what a deployment runs; the two binaries explain its shape.

```bash
# inside the container: the bridge the platform tunnels into
nanosec hypervisor --user <id> --hypervisor-addr :9090

# the turn loop for that user
nanosec agent --user <id> --agent <agent-id> --bus-url http://nanobus:9000
```

`nanosec home reconcile` rebuilds the workspace skeleton from the template
(`AGENT.md`, `bootstrap/`, `memory/`) without touching what the agent wrote.

## Runtime base image

The service image starts from
`kobeyoung81/nanoagent-runtime:24.04-node26.13.1-go1.27.4-r1`, a tool layer built
once from `runtime-base/Dockerfile`: the Ubuntu packages (including the .NET
runtime dependencies officecli needs), nvm/Node, Go, md2pdf, the common CLI tools
and the pinned officecli binary. Keeping that layer separate means a service
build is a few `COPY` instructions instead of a full toolchain install, and the
build host no longer reaches GitHub for officecli (whose release download is slow
and unreliable there).

Build it on the same Docker daemon the CI uses:

```
docker build -t kobeyoung81/nanoagent-runtime:24.04-node26.13.1-go1.27.4-r1 nanoagent/runtime-base
```

It is published to Docker Hub (public, like the service images) and also kept
locally, which is what the CI hits first. Toolchain upgrades follow one rule:
change the ARGs, bump the revision suffix (`-r2`), build, point this module's
`Dockerfile` at the new tag, and keep the previous tag for rollback. Never rebuild
an existing tag — it names a set of contents.

## Configuration

| Variable | Meaning |
|---|---|
| `NANOSEC_AGENT_BUS_URL` | The bus, for messages and the event stream |
| `NANOSEC_AGENT_TUNNEL_URL` | Where the hypervisor registers its tunnel (provisioner, else the bus) |
| `NANOSEC_PORTAL_URL` | Where this agent's row and settings live |
| `NANOSEC_ADMIN_URL` | Configuration and the catalogue |
| `NANOSEC_BUS_INTERNAL_TOKEN` | Shared secret for every internal surface |

Keys, models, tool belts and quotas are configuration, not environment variables:
they arrive from the admin module and are edited in the console.

## Design decisions

- **The container is the boundary.** A user's files, memory and transcript never
  leave it, except through the tunnel the platform owns.
- **The transcript is the authority.** The console keeps a projection for the UI;
  if it is lost, it is rebuilt from the JSONL in the volume.
- **Tools are declared, then granted.** The catalogue says what exists; the agent's
  belt says what this role may use — a tool that is not in the belt is not offered
  to the model at all.
- **Compaction is part of the loop.** A long conversation is summarised into a new
  segment rather than truncated, so the model keeps the thread.

## Docs

- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) — the platform and where the agent sits in it
- [`docs/BUNDLED_SKILLS_DESIGN.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/BUNDLED_SKILLS_DESIGN.md) — how the catalogue is seeded
- [`docs/BUNDLED_PROMPTS_DESIGN.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/BUNDLED_PROMPTS_DESIGN.md) — the same for prompts
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) — every document

## Status

Released as part of `v0.1.0`; the image digest is pinned in the super-repo's
`versions.lock`.

## Contributing and security

[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)
for setup and commit rules; report vulnerabilities privately through
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md), never
in a public issue.

## License

[AGPL-3.0](./LICENSE). The name and logo are covered separately — see
[`NOTICE`](./NOTICE).
