# Implementation Roadmap

**Version**: 2.2
**Date**: 2026-07-30
**Status**: Draft

This document outlines the phased implementation plan for nanosec.

---

## Overview

| Phase | Duration | Focus | Milestone |
|-------|----------|-------|-----------|
| 1 | Weeks 1-2 | Foundation | Project skeleton, core types, MySQL schema |
| 2 | Weeks 3-4 | Bus + Protocol | Working bus service with WebSocket |
| 3 | Weeks 5-6 | Agent Core | Working agent with LLM + tools |
| 4 | Weeks 7-8 | Gateway | Platform adapters, user resolution |
| 5 | Weeks 9-10 | Advanced | Cron, subagents, skills |
| 6 | Weeks 11-12 | Polish | Production ready, multi-user tested |

---

## Phase 1: Foundation (Weeks 1-2)

**Goal**: Project skeleton, core types, and database schema

### Sprint 1.1: Project Setup (Week 1)

| Task | Description | Priority |
|------|-------------|----------|
| Initialize Go module | `go mod init github.com/user/nanosec` | P0 |
| Create directory structure | `cmd/nanosec/`, `internal/`, `docs/`, `test/`, `migrations/` | P0 |
| Set up CI/CD | GitHub Actions for test, lint, build | P0 |
| Configure linting | golangci-lint with strict config | P0 |
| Create Makefile | `make build`, `make test`, `make lint`, `make migrate` | P1 |
| Add .gitignore | Go-specific ignores | P1 |

**Deliverables:**
- [ ] `go.mod` and `go.sum`
- [ ] Directory structure with `cmd/nanobus/`, `cmd/nanogateway/`, `cmd/nanosec/`
- [ ] `.github/workflows/ci.yml`
- [ ] `.golangci.yml`
- [ ] `Makefile`

### Sprint 1.2: Core Types + MySQL Schema (Week 2)

| Task | Description | Priority |
|------|-------------|----------|
| Design MySQL schema | users, user_channels, inbound_queue, outbound_queue | P0 |
| Create migrations | SQL migration files in `migrations/` | P0 |
| Implement protocol/messages.go | WebSocket message types | P0 |
| Implement config/config.go | Per-service configuration structs | P0 |
| Implement config/loader.go | Viper-based config loading | P0 |
| Write unit tests | 80%+ coverage for protocol, config | P0 |

**Deliverables:**
- [ ] `migrations/001_create_users.sql`
- [ ] `migrations/002_create_queues.sql`
- [ ] `internal/protocol/` package
- [ ] `internal/config/` package
- [ ] Unit tests passing
- [ ] CI pipeline green

**Code Examples:**

```go
// protocol/messages.go
const (
    TypeResolveUser  = "resolve_user"
    TypePostInbound  = "post_inbound"
    TypePollInbound  = "poll_inbound"
    TypePostOutbound = "post_outbound"
    TypePollOutbound = "poll_outbound"
)

type Message struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
    Error   string          `json:"error,omitempty"`
}

type InboundMessage struct {
    UserID    string         `json:"user_id"`
    SessionID string         `json:"session_id"`
    Channel   string         `json:"channel"`
    ChatID    string         `json:"chat_id"`
    SenderID  string         `json:"sender_id"`
    Content   string         `json:"content"`
    Media     []string       `json:"media,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
    Timestamp time.Time      `json:"timestamp"`
}
```

---

## Phase 2: Bus Service (Weeks 3-4)

**Goal**: Working bus service with WebSocket server and MySQL storage

### Sprint 2.1: Bus Storage Layer (Week 3)

| Task | Description | Priority |
|------|-------------|----------|
| Implement bus/store/db.go | MySQL connection pool | P0 |
| Implement bus/store/user.go | User registry CRUD | P0 |
| Implement bus/store/queue.go | Inbound/outbound queue operations | P0 |
| Implement atomic claiming | Prevent duplicate message processing | P0 |
| Write unit tests | Mock DB tests, 80%+ coverage | P0 |

**Deliverables:**
- [ ] `internal/bus/store/` package
- [ ] User registration and lookup
- [ ] Queue operations with atomic claiming
- [ ] Unit tests with mock database

**Dependencies:**
```go
require github.com/go-sql-driver/mysql v1.7.0
```

### Sprint 2.2: Bus WebSocket Server (Week 4)

| Task | Description | Priority |
|------|-------------|----------|
| Implement bus/server.go | WebSocket server | P0 |
| Implement bus/handler.go | Message handlers | P0 |
| Implement `nanobus` command | Bus executable | P0 |
| Integration tests | WebSocket + MySQL integration | P0 |
| E2E test | Bus running with test client | P0 |

**Deliverables:**
- [ ] `internal/bus/` package complete
- [ ] `cmd/nanobus/main.go` executable
- [ ] WebSocket server handling all message types
- [ ] Integration tests passing

**Dependencies:**
```go
require github.com/gorilla/websocket v1.5.0
```

**Milestone M1**: Bus service running

```bash
$ nanobus --config ~/.nanosec/bus.json
INFO bus started port=9000
```

---

## Phase 3: Agent Core (Weeks 5-6)

**Goal**: Working agent with LLM, tools, and sessions

### Sprint 3.1: Agent + Provider + Session (Week 5)

| Task | Description | Priority |
|------|-------------|----------|
| Implement provider/provider.go | Provider interface | P0 |
| Implement provider/openai.go | OpenAI-compatible client | P0 |
| Implement session/session.go | Session struct | P0 |
| Implement session/store.go | JSONL file storage | P0 |
| Implement agent/agent.go | Agent struct and Run() | P0 |
| Implement agent/client.go | WebSocket client to bus | P0 |
| Implement agent/loop.go | Message processing loop | P0 |

**Deliverables:**
- [ ] `internal/provider/` package
- [ ] `internal/session/` package with JSONL storage
- [ ] `internal/agent/` package
- [ ] Agent polling from bus

**Dependencies:**
```go
require github.com/sashabaranov/go-openai v1.20.0
```

### Sprint 3.2: Tools System (Week 6)

| Task | Description | Priority |
|------|-------------|----------|
| Implement tool/tool.go | Tool interface | P0 |
| Implement tool/registry.go | Tool registry | P0 |
| Implement tool/shell.go | Shell execution with timeout | P0 |
| Implement tool/filesystem.go | Read, write, edit, list | P0 |
| Implement tool/message.go | Send messages via bus | P0 |
| Implement tool/web.go | Web search (Brave), fetch | P1 |
| Security tests | Path traversal, sandbox | P0 |
| Implement `nanosec` command | Agent executable with --user flag | P0 |

**Deliverables:**
- [ ] `internal/tool/` package
- [ ] All core tools implemented
- [ ] `cmd/nanosec/main.go` agent executable
- [ ] Security test suite

**Milestone M2**: Agent processing messages from bus

```bash
$ nanosec --user=alice --config ~/.nanosec/agent.json
INFO agent started user=alice instance=abc123
```

---

## Phase 4: Gateway (Weeks 7-8)

**Goal**: Gateway service with platform adapters and user resolution

### Sprint 4.1: Gateway Core + CLI (Week 7)

| Task | Description | Priority |
|------|-------------|----------|
| Implement gateway/gateway.go | Main gateway logic | P0 |
| Implement gateway/client.go | WebSocket client to bus | P0 |
| Implement gateway/channel/channel.go | Channel interface | P0 |
| Implement gateway/channel/cli/ | CLI adapter (stdin/stdout) | P0 |
| Implement `nanogateway` command | Gateway executable | P0 |
| Implement `nanosec chat` command | Standalone CLI chat mode | P0 |
| Manual E2E testing | Full flow: CLI → Gateway → Bus → Agent | P0 |

**Deliverables:**
- [ ] `internal/gateway/` package
- [ ] `internal/gateway/channel/` package
- [ ] Working CLI adapter
- [ ] `cmd/nanogateway/main.go` executable
- [ ] `cmd/nanosec/chat.go` standalone mode
- [ ] E2E test with all three processes

### Sprint 4.2: WeCom + Feishu + WeChat KF (Week 8)

| Task | Description | Priority |
|------|-------------|----------|
| Implement gateway/channel/wecom/ | WeCom (Enterprise WeChat) adapter | P1 |
| Implement gateway/channel/feishu/ | Feishu adapter | P2 |
| Implement gateway/channel/wechatkf/ | WeChat KF (Customer Service) adapter | P1 |
| Multi-channel testing | Run gateway with multiple channels | P1 |
| Multi-user testing | Multiple users, separate agents | P0 |

**Deliverables:**
- [ ] WeCom adapter
- [ ] Feishu adapter
- [ ] WeChat KF adapter
- [ ] Multi-user E2E tests passing

**Dependencies:**
```go
require github.com/xen0n/go-workwx v2.0.0-alpha.1
require github.com/larksuite/oapi-sdk-go/v3 v3.5.3
```

**Milestone M3**: Full three-process system working with CLI adapter

---

## Phase 5: Advanced Features (Weeks 9-10)

**Goal**: Feature parity with Python nanobot, multi-user production ready

### Sprint 5.1: Cron + Subagents (Week 9)

| Task | Description | Priority |
|------|-------------|----------|
| Implement cron/service.go | Cron scheduler | P1 |
| Implement cron/job.go | Job persistence | P1 |
| Complete tool/cron.go | Full cron tool | P1 |
| Implement agent/subagent.go | Background agents | P1 |
| Complete tool/spawn.go | Full spawn tool | P1 |
| Integration tests | Scheduled + background tasks | P1 |

**Deliverables:**
- [ ] `internal/cron/` package
- [ ] Working cron scheduling
- [ ] Subagent spawning
- [ ] CLI: `nanosec cron add/list/remove`

**Dependencies:**
```go
require github.com/robfig/cron/v3 v3.0.1
```

### Sprint 5.2: Skills System (Week 10)

| Task | Description | Priority |
|------|-------------|----------|
| Implement skill/loader.go | Load skill definitions | P2 |
| Implement skill/registry.go | Skill registry | P2 |
| Migrate bundled skills | GitHub, weather, tmux | P2 |
| Context integration | Inject skills into prompts | P2 |
| Voice transcription | Complete Groq Whisper | P2 |

**Deliverables:**
- [ ] `internal/skill/` package
- [ ] Bundled skills working
- [ ] Voice transcription for Telegram

---

## Phase 6: Polish (Weeks 11-12)

**Goal**: Production readiness

### Sprint 6.1: CLI + UX (Week 11)

| Task | Description | Priority |
|------|-------------|----------|
| Complete CLI | All commands for each binary | P0 |
| Add `onboard` command | Interactive setup for bus, gateway, agent | P1 |
| Add `status` command | Show system status (bus, agents, gateways) | P1 |
| Add `user` commands | User management (add, list, link-channel) | P0 |
| Rich terminal output | Colors, formatting | P2 |
| Help and documentation | `--help` for all commands | P0 |

**Deliverables:**
- [ ] Complete CLI with all commands
- [ ] Interactive onboarding
- [ ] User management commands
- [ ] Rich terminal output

**Dependencies:**
```go
require github.com/spf13/cobra v1.8.0
```

**CLI Commands:**
```
nanobus                  # Start bus service
├── version              # Print version
└── --config/-c          # Config file path

nanogateway              # Start gateway service
├── version              # Print version
├── --config/-c          # Config file path
└── --message/-m         # One-shot message mode

nanosec                  # Start agent for a user
├── version              # Print version
├── --user/-u            # Required: user ID
└── --config/-c          # Config file path
```

### Sprint 6.2: Release (Week 12)

| Task | Description | Priority |
|------|-------------|----------|
| goreleaser setup | Multi-platform binaries | P0 |
| Docker image | Dockerfile + docker-compose | P1 |
| README.md | Installation, usage, config | P0 |
| CHANGELOG.md | Version history | P1 |
| LICENSE | MIT license | P0 |
| Release v0.1.0 | First public release | P0 |

**Deliverables:**
- [ ] `.goreleaser.yaml`
- [ ] `Dockerfile`
- [ ] `docker-compose.yml`
- [ ] Complete documentation
- [ ] GitHub release v0.1.0

**Milestone M4**: Feature parity with Python nanobot

---

## Milestones Summary

| Milestone | Week | Criteria |
|-----------|------|----------|
| **M1** | 4 | Bus service running with WebSocket + MySQL |
| **M2** | 6 | Agent processing messages from bus with tools |
| **M3** | 8 | Full three-process system (Gateway + Bus + Agent) with CLI |
| **M4** | 12 | Multi-user production ready, all channels working |

---

## Success Criteria

| Metric | Target | How to Measure |
|--------|--------|----------------|
| Binary size | <20MB | `ls -lh nanosec` |
| Startup time | <100ms | Benchmark test |
| Memory (idle) | <50MB | `ps aux` RSS |
| Test coverage | >80% | `go test -cover` |
| Core LOC | <5,000 | `cloc internal/` |
| All P0 features | 100% | Checklist |
| All P1 features | 100% | Checklist |

---

## Risk Mitigation

| Risk | Mitigation |
|------|------------|
| LLM API changes | Abstract behind provider interface |
| Telegram API rate limits | Implement backoff/retry |
| WhatsApp bridge complexity | Make it optional, document setup |
| Scope creep | Strict adherence to phases |
| Concurrency bugs | Run tests with `-race` |
| MySQL availability | Document setup, provide docker-compose |
| WebSocket reliability | Implement reconnection logic with backoff |
| Multi-user isolation | Per-user queues, proper user_id validation |

---

## Dependencies Timeline

```
Week 1-2: stdlib only
Week 3-4: + go-sql-driver/mysql, gorilla/websocket, spf13/viper
Week 5-6: + sashabaranov/go-openai
Week 7-8: + xen0n/go-workwx, larksuite/oapi-sdk-go
Week 9-10: + robfig/cron/v3
Week 11-12: + cobra
```

---

## Post-Launch

After v0.1.0:
- [ ] Performance optimization
- [ ] Additional LLM providers (native Anthropic, multi-provider registry)
- [ ] Plugin system for external tools
- [ ] Web UI (optional)
- [ ] Metrics and observability

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
| 2.1 | 2026-02-22 | Claude | Fixed Week 7-8 dependency references (telegram/discord -> go-workwx/oapi-sdk-go) |
| 2.2 | 2026-07-30 | Claude | Fixed dependency names (gocron -> robfig/cron/v3); added WeChat KF to Phase 4; updated channel descriptions |
