# Package Specifications

**Version**: 2.0
**Date**: 2026-07-30
**Status**: Current

This document provides specifications for each Go package across all MyriadMind modules.

---

## nanoprotocol (shared protocol library)

**Module**: `gitee.com/kobeyoung81/nanoprotocol`
**Package**: `nanoprotocol`
**Purpose**: WebSocket wire protocol shared by all services.
**Dependencies**: `gorilla/websocket` + stdlib only.

### Files

| File | Purpose |
|------|---------|
| `messages.go` | Message envelope, 25 type constants, all request/response structs, `NewMessage`, `NewErrorMessage`, `DecodePayload` |
| `client.go` | WebSocket `Client` with auto-reconnect, 17 typed RPC methods |

### Key Design Rules
- No imports beyond `gorilla/websocket` and stdlib
- New shared types go here; service-specific logic stays in service repos
- All `Type*` constants follow `component_direction_action` naming

---

## nanobus (bus service)

**Module**: `gitee.com/kobeyoung81/nanobus`
**Purpose**: WebSocket message broker with MySQL backend.
**Dependencies**: `nanoprotocol`, `go-sql-driver/mysql`, `gorilla/websocket`, `cobra`, `viper`, `uuid`, `sqlmock` (test)

### `cmd/nanobus` — CLI

CLI commands: `nanobus` (serve), `nanobus version`, `nanobus user add|list|link`, `nanobus config seed|get|set`, `nanobus migrate up|status`

### `internal/server` — WebSocket Server

`server.go`: `Server` struct, `NewServer`, `ListenAndServe`, `Close`, `Addr`
`handler.go`: Type switch dispatching all `nanoprotocol` message types to store operations. Handles: `TypeResolveUser`, `TypePostInbound`, `TypePollInbound`, `TypePostOutbound`, `TypePollOutbound`, `TypePollPendingContainers`, `TypeUpdateContainerStatus`, `TypeRegisterRunner`, `TypeUpdateUserStatus`, `TypeGetUserInfo`, `TypeGetWeChatKFSyncState`, `TypeUpdateWeChatKFCursor`, `TypeUpdateWeChatKFCallbackTok`, `TypeGetConfigItems`, `TypeSetConfigItem`

### `internal/store` — Data Access Layer

| File | Purpose |
|------|---------|
| `db.go` | `Store` struct, `Open()`, `New()`, `Close()`, `DB()` |
| `user.go` | User registry: `RegisterUser`, `LinkChannel`, `ResolveUser`, `ResolveUserBinding`, `ListUsers`, `UpdateUserStatus`, `GetUserInfo`, `EnableChannel`, `DisableChannel`, `PollPendingContainers`, `UpdateContainerStatus`. Sentinel errors: `ErrUserNotFound`, `ErrChannelDisabled` |
| `queue.go` | Message queues: `PostInbound`, `ClaimInbound` (SELECT FOR UPDATE), `PostOutbound`, `ClaimOutbound` (SELECT FOR UPDATE) |
| `wechatkf_state.go` | WeChat KF sync state: `GetWeChatKFSyncState`, `UpdateWeChatKFCursor`, `UpdateWeChatKFCallbackToken` |
| `runner.go` | Runner registry: `RegisterRunner`, `UpdateRunnerHeartbeat` |
| `config.go` | Service config CRUD: `GetConfigItems`, `GetConfigItem`, `SetConfigItem`, `SeedConfigItems`, `ConfigItemsToMap` |
| `migrate.go` | Migration engine: `Migrate`, `MigrationStatus`; embedded SQL files applied in lexicographic order |

### `internal/config` — Configuration

`BusConfig` { `Server` {Host, Port}, `Database` {Driver, DSN} }. Constants: `DefaultBusHost`, `DefaultBusPort`, `DefaultDBDriver`. Functions: `DefaultBusConfig`, `LoadBusConfig`, `ValidateBusConfig`.

### `internal/log` — Logging

Single constant: `LevelVerbose = slog.Level(-8)`.

### `migrations/` — SQL Migrations

12 migrations (001-012), embedded via `go:embed` in `embed.go`:
001: users + user_channels
002: inbound_queue + outbound_queue
003: status + level on users
004: container_requests
005: media JSON on outbound_queue
006: message_id columns + indexes
007: wechatkf_sync_state
008: service_configs (dropped by 009)
009: service_config_items (key-value replacement)
010: enabled flag on user_channels
011: runners
012: binding_id, provider, binding_status, agent_id

---

## nanogateway (gateway service)

**Module**: `gitee.com/kobeyoung81/nanogateway`
**Purpose**: Chat platform adapters, user resolution, message routing.
**Dependencies**: `nanoprotocol`, `gorilla/websocket`, `cobra`, `uuid`, `go-workwx/v2` (fork), `oapi-sdk-go/v3`

### `cmd/nanogateway` — CLI

Flags: `--bus-url` (env: `NANOSEC_GATEWAY_BUS_URL`), `--message` (one-shot CLI mode). Subcommand: `nanogateway version`.

### `internal/gateway` — Gateway Engine

`gateway.go`: `Gateway` struct, `New()`, `Run()` (starts channels, polls outbound every 500ms per channel), `handleInbound()` (resolve user via bus, optional ack, post inbound)
`client.go`: `BusClient` wrapping `nanoprotocol.Client`; methods: `ResolveUser`, `PostInbound`, `PollOutbound`, `Close`

### `internal/channel` — Channel Abstraction

| File | Purpose |
|------|---------|
| `channel.go` | `InboundMessage`, `OutboundMessage` types; `InboundHandler` type; `Channel` interface (`Name`, `Start`, `Send`, `Stop`) |
| `cli/adapter.go` | stdin/stdout adapter with one-shot mode |
| `wecom/adapter.go` | WeCom (Enterprise WeChat) HTTP callback via go-workwx fork |
| `feishu/adapter.go` | Feishu (Lark) WebSocket via oapi-sdk-go/v3 |
| `wechatkf/adapter.go` | WeChat KF polling adapter with cursor-based sync |
| `wechatkf/callback.go` | AES-256-CBC message decryption, URL verification, event notification |

### `internal/config` — Configuration

`GatewayConfig` { `Channels` {CLI, WeCom, Feishu, WeChatKF}, `Ack` {Enabled, Text} }.
Constants: `DefaultBusURL`, `DefaultWeComCallbackAddr`, `DefaultWeChatKFCallbackAddr`.
Functions: `ResolveBusURL`, `LoadGatewayConfig`, `ValidateGatewayConfig`.

### `go-workwx/` — Vendored WeCom API

Fork of `github.com/xen0n/go-workwx/v2` with patches for nanogateway's callback handling.

---

## nanosec (agent service)

**Module**: `gitee.com/kobeyoung81/nanosec`
**Purpose**: Per-user AI agent with ReAct loop, LLM, tools, sessions, skills.
**Dependencies**: `nanoprotocol`, `gorilla/websocket`, `cobra`, `go-openai`, `robfig/cron/v3`, `uuid`

### `cmd/nanosec` — CLI

Flags: `--user` (required), `--bus-url` (env: `NANOSEC_BUS_URL`). Subcommand: `nanosec version`.

### `internal/agent` — Agent Core

| File | Purpose |
|------|---------|
| `agent.go` | `Config`, `Agent` struct, `New()`, `Run()`, `SetContextBuilder()` |
| `loop.go` | `processMessage()` — ReAct loop (context -> LLM -> tools -> loop) |
| `context.go` | `ContextBuilder` — assembles system prompt from identity, bootstrap, memory, skills |
| `budget.go` | Token budget estimation and message truncation |
| `subagent.go` | `SubagentManager` — spawns isolated subagents; `Spawn()` |
| `summarize.go` | `Summarizer` — async LLM-based session summarization |

`Agent` holds: config, busClient (`BusClient` interface), provider (`Provider` interface), tools (`*tool.Registry`), sessions (`*session.Store`), contextBuilder (`*ContextBuilder`), summarizer (`*Summarizer`).

### `internal/provider` — LLM Provider

`provider.go`: `Provider` interface (`Chat`), `ChatRequest`, `Message`, `ToolCall`, `ToolDef`, `FunctionDef`, `ChatResponse`, `Usage`
`openai.go`: `OpenAIProvider` implementing `Provider` via `sashabaranov/go-openai` with logging HTTP transport
`mock.go`: `MockProvider` for testing

### `internal/tool` — Tool System

| File | Purpose |
|------|---------|
| `tool.go` | `Tool` interface, `ContextualTool` interface, `ParameterSchema`, `PropertySchema` |
| `registry.go` | `Registry` — thread-safe tool registration, execution, OpenAPI definition generation |
| `shell.go` | `bash` tool — command execution with timeout, deny list, workspace sandboxing |
| `filesystem.go` | `read_file`, `write_file`, `edit_file`, `ls`, `append_note` — all with `safePath()` |
| `web.go` | `web_search` (Tavily API), `web_fetch` |
| `message.go` | `send_message` — posts via bus `BusClient` interface |
| `spawn.go` | `spawn` — delegates to `SubagentManager.Spawn()` |
| `cron.go` | `cron` add/list/remove — delegates to `CronScheduler` interface |
| `media_download.go` | `media_download` — WeCom media download via go-workwx API |
| `media_upload.go` | `media_upload` — WeCom media upload and message delivery |

### `internal/session` — Session Storage

`sessions.go`: `Session` struct (thread-safe via `sync.RWMutex`), `New()`, `AddMessage()`, `GetMessages()`, `MessageCount()`, `SetSummary()/GetSummary()`, `SetMeta()/GetMeta()`, `TruncateMessages()`, `Clear()`
`store.go`: `Store` — file-backed JSONL storage with in-memory cache, `GetOrCreate()`, `ActiveSession()` (finds latest version), `NewSession()` (creates incremented version), `ArchiveSession()`, `Save()`
Utility: `IsIdle()`, `ParseSessionKey()`

### `internal/skill` — Skill System

`loader.go`: `Loader` — reads `SKILL.md` files from workspace and builtin dirs; parses YAML frontmatter; checks requirements
`registry.go`: `Registry` — in-memory cache wrapping Loader; `Get()`, `List()`, `BuildSummary()`, `GetAlwaysSkills()`, `LoadForContext()`
`builtin/weather/SKILL.md`: Built-in weather skill (embedded via `embed.FS`)

### `internal/cron` — Job Scheduler

`service.go`: `Service` — wraps `robfig/cron/v3` with JSON file persistence; `Start()`, `Stop()`, `AddJob()`, `RemoveJob()`, `ListJobs()`
`job.go`: `Job`, `Schedule`, `ScheduleKind` (`at`/`every`/`cron`), `Payload`, `JobState`, `JobCallback`

### `internal/config` — Configuration

`config.go`: `AgentConfig`, `ProviderConfig`, `AgentCoreConfig`, `ToolsConfig`, `ShellConfig`, `WebConfig`, `MediaConfig`
`defaults.go`: `DefaultAgentConfig()`, default constants (`DefaultBusURL`, `DefaultProviderModel`, etc.)
`loader.go`: `LoadAgentConfig()` (fetches from bus via `nanoprotocol.GetConfigItems`), `ResolveBusURL()`, `ValidateAgentConfig()`
`paths.go`: `PathsConfig` (workspace, bootstrap, memory, skills, sessions, cron paths), `ResolvedPaths`, `Resolve()`

### `internal/log` — Logging

Single constant: `LevelVerbose = slog.Level(-8)`.

### `internal/messageid` — Message IDs

`New()` returns a UUID string for message identification.

---

## nanorunner (runner service)

**Module**: `gitee.com/kobeyoung81/nanorunner`
**Purpose**: Container provisioning via Docker SDK.
**Dependencies**: `nanoprotocol`, `cobra`, `docker/docker`

### `cmd/nanorunner` — CLI

Flags: `--bus-url` (env: `NANOSEC_RUNNER_BUS_URL`). Subcommand: `nanorunner version`.

### `internal/runner` — Container Management

`runner.go`: `Runner` struct, `New()`, `Run()` (poll loop), `DisposeContainer()`
`docker.go`: `DockerClient` interface, `RealDockerClient` (Docker SDK wrapper), `NewDockerClient()`
`quotas.go`: `Quota` struct (`MemoryMB`, `CPUs`), `QuotaForLevel()` returns tier limits (free: 512MB/1CPU, pro: 2048MB/2CPU, ultra: 4096MB/4CPU)

### `internal/config` — Configuration

`RunnerConfig` { `Runner` {PollInterval, RunnerID}, `AgentImage`, `BusURL`, `Network` }.
Constants: `DefaultBusURL`.
Functions: `ResolveBusURL`, `LoadRunnerConfig`, `ValidateRunnerConfig`.

Container naming: `nanosec-agent-{userID}` or `nanosec-agent-{userID}-{bindingID}`.

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft (single-process architecture with `internal/bus/`, `internal/channel/`) |
| 2.0 | 2026-07-30 | Claude | Full rewrite for 5-module decoupled architecture; documented actual packages across all services; removed outdated bus/channel/provider-registry packages; added ContextualTool, SubagentManager, ConfigItems, DockerClient, Skill Registry |
