# Architecture Design Document: Nanosec

**Version**: 2.0
**Date**: 2026-02-09
**Status**: Draft

> **过时提示（2026-10-18）**：本文档继承自上游项目，描述「单镜像 + systemd」的部署形态。
> 平台的权威架构与部署说明在超级仓库的 `docs/ARCHITECTURE.md` / `docs/DEPLOYMENT.md`。
> 存储已从 MySQL 迁到 **MongoDB**（由 `nanobus` 独占，其他服务经总线访问），因此下文
> 所有 MySQL 描述——特别是「MySQL Schema」那一节——都是迁移前的历史记录，不再是现状。

---

## 1. System Overview

### 1.1 High-Level Architecture

Nanosec uses a **decoupled four-process architecture** for multi-user support and horizontal scaling.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                              External Services                                │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐  ┌──────────────┐     │
│  │   CLI    │ │  WeCom   │ │  Feishu  │ │WeChat KF │  │ LLM Provider │     │
│  │ (stdin)  │ │(Ent.WeChat)│ │          │ │(Customer)│  └──────┬───────┘     │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘         │             │
└───────┼────────────┼────────────┼────────────┼───────────────┼─────────────┘
        │            │            │            │               │
        └────────────┴─────┬──────┴────────────┘               │
                           │                         │
                           ▼                         │
┌────────────────────────────────────────────────────┼─────────────────────────┐
│                    GATEWAY (nanogateway)            │                         │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐           │                         │
│  │   CLI    │ │  WeCom   │ │  Feishu  │           │                         │
│  │ Adapter  │ │ Adapter  │ │ Adapter  │           │                         │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘           │                         │
│       └────────────┴─────┬──────┘                  │                         │
│                          │ Resolve user, post/poll               │           │
└──────────────────────────┼───────────────────────────────────────┼───────────┘
                           │ WebSocket                             │
                           ▼                                       │
┌──────────────────────────────────────────────────────────────────┼───────────┐
│                      BUS (nanobus)                                │           │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │           │
│  │  User Registry  │  │ Per-User Queues │  │   WebSocket     │  │           │
│  │  (Mongo)        │  │ (Mongo)         │  │   Server        │  │           │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │           │
└──────────────────────────┬───────────────────────────────────────┼───────────┘
                           │ WebSocket                             │
           ┌───────────────┼───────────────┐                       │
           │               │               │                       │
           ▼               ▼               ▼                       │
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐       │
│ AGENT (alice)   │ │ AGENT (bob)     │ │ AGENT (carol)   │       │
│                 │ │                 │ │                 │       │
│ nanosec         │ │ nanosec         │ │ nanosec         │       │
│ --user=alice    │ │ --user=bob      │ │ --user=carol    │       │
│                 │ │                 │ │                 │       │
│ ┌─────────────┐ │ │ ┌─────────────┐ │ │ ┌─────────────┐ │       │
│ │ Sessions    │ │ │ │ Sessions    │ │ │ │ Sessions    │ │◄──────┘
│ │ (JSONL)     │ │ │ │ (JSONL)     │ │ │ │ (JSONL)     │ │  LLM calls
│ └─────────────┘ │ │ └─────────────┘ │ │ └─────────────┘ │
│ ┌─────────────┐ │ │ ┌─────────────┐ │ │ ┌─────────────┐ │
│ │ Tools       │ │ │ │ Tools       │ │ │ │ Tools       │ │
│ └─────────────┘ │ │ └─────────────┘ │ │ └─────────────┘ │
└─────────────────┘ └─────────────────┘ └─────────────────┘

┌─────────────────────────────────────────────────────────┐
│                  RUNNER (nanorunner)                     │
│  Polls bus for container requests → provisions Docker   │
│  containers per user, enforces quota tiers              │
└─────────────────────────────────────────────────────────┘
         │
         ▼
      Docker Engine
```

### 1.2 Process Model

| Process | Binary | Responsibility | Scaling |
|---------|--------|----------------|---------|
| **Bus** | `nanobus` | User registry, message queues, WebSocket server | Multiple behind LB (stateless) |
| **Gateway** | `nanogateway` | Platform adapters, user resolution, message routing | Multiple per channel |
| **Agent** | `nanosec --user=X` | LLM calls, tools, sessions | One per user |
| **Runner** | `nanorunner` | Container provisioning, Docker management, quota tiers | Multiple behind LB |

### 1.3 Communication

| From | To | Protocol | Purpose |
|------|-----|----------|---------|
| Gateway | Bus | WebSocket | Post inbound, poll outbound, resolve user |
| Agent | Bus | WebSocket | Poll inbound, post outbound |
| Runner | Bus | WebSocket | Poll container requests, post status updates |
| Agent | LLM | HTTPS | Chat completions |
| Gateway | Platforms | Platform-specific | CLI, WeCom, Feishu, WeChat KF |
| Runner | Docker | Docker SDK | Provision/dispose containers, query status |

---

## 2. Go Package Structure

```
nanosec/
├── cmd/
│   ├── nanobus/                      # Bus executable
│   │   └── main.go                   # nanobus root command
│   ├── nanogateway/                  # Gateway executable
│   │   └── main.go                   # nanogateway root command
│   ├── nanosec/                      # Agent executable
│   │   └── main.go                   # nanosec root command (--user flag)
│   └── nanorunner/                   # Runner executable
│       └── main.go                   # nanorunner root command
│
├── internal/                      # Private packages
│   │
│   ├── bus/                       # Bus service
│   │   ├── server.go              # WebSocket server
│   │   ├── handler.go             # Message handlers
│   │   └── store/                 # MongoDB storage
│   │       ├── db.go              # Database connection
│   │       ├── user.go            # User registry
│   │       └── queue.go           # Message queues
│   │
│   ├── gateway/                   # Gateway service
│   │   ├── gateway.go             # Main gateway logic
│   │   ├── client.go              # WebSocket client to bus
│   │   └── channel/               # Platform adapters
│   │       ├── channel.go         # Channel interface
│   │       ├── cli/
│   │       │   └── adapter.go     # CLI adapter (stdin/stdout)
│   │       ├── wecom/
│   │       │   └── adapter.go     # WeCom (Enterprise WeChat)
│   │       ├── feishu/
│   │       │   └── adapter.go     # Feishu implementation
│   │       └── wechatkf/
│   │           └── adapter.go     # WeChat KF (Customer Service) polling adapter
│   │
│   ├── agent/                     # Agent service
│   │   ├── agent.go               # Agent struct and Run()
│   │   ├── client.go              # WebSocket client to bus
│   │   ├── loop.go                # Message processing loop
│   │   ├── context.go             # Context/prompt builder
│   │   └── subagent.go            # Background agent spawning
│   │
│   ├── session/                   # File-based session storage
│   │   ├── session.go             # Session struct
│   │   └── store.go               # JSONL file storage
│   │
│   ├── provider/                  # LLM provider
│   │   ├── provider.go            # Provider interface
│   │   ├── openai.go              # OpenAI-compatible client
│   │   └── types.go               # ChatRequest, ChatResponse
│   │
│   ├── tool/                      # Agent tools
│   │   ├── tool.go                # Tool interface
│   │   ├── registry.go            # Tool registry
│   │   ├── shell.go               # Shell execution
│   │   ├── filesystem.go          # File operations
│   │   ├── web.go                 # Web search/fetch
│   │   ├── message.go             # Send messages via bus
│   │   ├── cron.go                # Schedule tasks
│   │   ├── spawn.go               # Spawn subagents
│   │   ├── media_download.go      # WeCom media download
│   │   └── media_upload.go        # WeCom media upload
│   │
│   ├── protocol/                  # Shared WebSocket protocol
│   │   ├── messages.go            # Message type definitions
│   │   └── client.go              # Base WebSocket client
│   │
│   ├── cron/                      # Scheduled tasks
│   │   ├── service.go             # Cron service
│   │   └── job.go                 # Job definition
│   │
│   ├── skill/                     # Skills/plugin system
│   │   ├── loader.go              # Load skill definitions
│   │   └── registry.go            # Skill registry
│   │
│   └── config/                    # Configuration
│       ├── config.go              # Config structs
│       ├── loader.go              # Load from file/env
│       └── defaults.go            # Default values
│
├── configs/                       # Default configuration files
│   ├── bus.example.json
│   ├── gateway.example.json
│   └── agent.example.json
│
│   (这个仓库里没有 migrations/：存储归 nanobus 独占，集合清单在
│    它的 `nanobus/migrations-mongo/` 下)
├── test/                          # Integration tests
│   ├── integration/
│   └── e2e/
│
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile
├── docker-compose.yml
├── .goreleaser.yaml
└── README.md
```

### 2.1 Package Dependencies

```
                    cmd/nanobus       cmd/nanogateway      cmd/nanosec
                         │                 │                    │
                         ▼                 ▼                    ▼
                   ┌───────────┐     ┌─────────────┐    ┌───────────┐
                   │ internal/ │     │  internal/  │    │ internal/ │
                   │   bus/    │     │   gateway/  │    │   agent/  │
                   └─────┬─────┘     └──────┬──────┘    └─────┬─────┘
          │                  │                 │
          │           ┌──────┴──────┐    ┌─────┴─────────┐
          │           │             │    │     │         │
          ▼           ▼             │    ▼     ▼         ▼
    ┌──────────┐  ┌────────┐       │  ┌────┐ ┌────────┐ ┌────────┐
    │ store/   │  │channel/│       │  │tool│ │session │ │provider│
    │ (Mongo)  │  └────────┘       │  └────┘ └────────┘ └────────┘
    └──────────┘                   │
                                   │
                          ┌────────┴────────┐
                          │                 │
                          ▼                 ▼
                    ┌──────────┐     ┌──────────┐
                    │ protocol │     │  config  │
                    └──────────┘     └──────────┘
```

**Dependency Rules:**
- Each `cmd/` binary imports only its corresponding `internal/` packages
- `bus/`, `gateway/`, `agent/`, `runner/` are independent - no cross-imports
- All four services share `protocol/` and `config/`
- `agent/` imports `session/`, `provider/`, `tool/`
- `gateway/` imports `channel/`
- `bus/` imports `store/`
- `runner/` imports Docker SDK (github.com/docker/docker)

---

## 3. Core Components

### 3.1 Bus Service (`internal/bus`)

**Purpose**: Central message routing with MongoDB-backed user registry and per-user queues.

#### Store schema (迁移前是 MySQL；现在的集合清单在 `nanobus/migrations-mongo/`)

> 下面这段 DDL 是 MySQL 时期的记录，**已不是现状**：账号与渠道现在内嵌在同一个文档里，
> 会话消息是真数组，队列与配置都是集合。现状以 `nanobus/internal/store` 与
> `nanobus/migrations-mongo/` 为准。

```sql
-- User registry
CREATE TABLE users (
    id          VARCHAR(255) PRIMARY KEY,  -- "alice", "bob"
    status      VARCHAR(64) DEFAULT 'active',
    level       INT DEFAULT 0,             -- quota tier
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

-- User channel identities
CREATE TABLE user_channels (
    user_id     VARCHAR(255) NOT NULL,
    channel     VARCHAR(64) NOT NULL,       -- "telegram", "discord"
    sender_id   VARCHAR(255) NOT NULL,      -- Platform's user ID
    enabled     TINYINT(1) DEFAULT 1,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (channel, sender_id),       -- One user per platform identity
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX idx_user (user_id)
);

-- Per-user inbound queue (created dynamically: inbound_alice, inbound_bob, etc.)
-- Or use single table with partitioning:
CREATE TABLE inbound_queue (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     VARCHAR(255) NOT NULL,
    session_id  VARCHAR(255) NOT NULL,      -- "telegram:123456"
    channel     VARCHAR(64) NOT NULL,
    chat_id     VARCHAR(255) NOT NULL,
    sender_id   VARCHAR(255) NOT NULL,
    content     TEXT NOT NULL,
    media       JSON,
    metadata    JSON,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    status      ENUM('pending', 'claimed', 'done') DEFAULT 'pending',
    claimed_by  VARCHAR(255),               -- Agent instance ID
    claimed_at  TIMESTAMP,

    INDEX idx_user_pending (user_id, status, created_at)
) PARTITION BY KEY(user_id) PARTITIONS 16;

-- Per-user outbound queue
CREATE TABLE outbound_queue (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     VARCHAR(255) NOT NULL,
    session_id  VARCHAR(255) NOT NULL,
    channel     VARCHAR(64) NOT NULL,
    chat_id     VARCHAR(255) NOT NULL,
    content     TEXT NOT NULL,
    media       JSON,                       -- []MediaItem attachments
    reply_to    VARCHAR(255),
    metadata    JSON,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    status      ENUM('pending', 'claimed', 'done') DEFAULT 'pending',
    claimed_by  VARCHAR(255),               -- Gateway instance ID
    claimed_at  TIMESTAMP,

    INDEX idx_channel_pending (channel, status, created_at),
    INDEX idx_user_pending (user_id, status, created_at)
) PARTITION BY KEY(user_id) PARTITIONS 16;

-- Container lifecycle requests (polled by nanorunner)
CREATE TABLE container_requests (
    id          VARCHAR(255) PRIMARY KEY,   -- UUID
    user_id     VARCHAR(255) NOT NULL,
    action      ENUM('provision', 'dispose') NOT NULL,
    status      ENUM('pending', 'processing', 'done', 'failed') DEFAULT 'pending',
    image       VARCHAR(255),
    container_id VARCHAR(255),
    error       TEXT,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_pending (status, created_at),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
```

#### WebSocket Server

```go
// bus/server.go
package bus

import (
    "net/http"

    "github.com/gorilla/websocket"
)

type Server struct {
    store    *store.Store
    upgrader websocket.Upgrader
}

func NewServer(store *store.Store) *Server {
    return &Server{
        store: store,
        upgrader: websocket.Upgrader{
            CheckOrigin: func(r *http.Request) bool { return true },
        },
    }
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    conn, err := s.upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }
    defer conn.Close()

    s.handleConnection(conn)
}

func (s *Server) handleConnection(conn *websocket.Conn) {
    for {
        var msg protocol.Message
        if err := conn.ReadJSON(&msg); err != nil {
            return
        }

        resp := s.handleMessage(msg)
        if resp != nil {
            conn.WriteJSON(resp)
        }
    }
}
```

#### Message Handlers

```go
// bus/handler.go
package bus

import "nanosec/internal/protocol"

func (s *Server) handleMessage(msg protocol.Message) *protocol.Message {
    switch msg.Type {
    case protocol.TypeResolveUser:
        return s.handleResolveUser(msg)
    case protocol.TypePostInbound:
        return s.handlePostInbound(msg)
    case protocol.TypePollInbound:
        return s.handlePollInbound(msg)
    case protocol.TypePostOutbound:
        return s.handlePostOutbound(msg)
    case protocol.TypePollOutbound:
        return s.handlePollOutbound(msg)
    case protocol.TypePostContainerRequest:
        return s.handlePostContainerRequest(msg)
    case protocol.TypePollContainerRequest:
        return s.handlePollContainerRequest(msg)
    case protocol.TypeUpdateContainerStatus:
        return s.handleUpdateContainerStatus(msg)
    case protocol.TypeUpdateUserStatus:
        return s.handleUpdateUserStatus(msg)
    default:
        return &protocol.Message{Type: protocol.TypeError, Error: "unknown type"}
    }
}

func (s *Server) handleResolveUser(msg protocol.Message) *protocol.Message {
    var req protocol.ResolveUserRequest
    json.Unmarshal(msg.Payload, &req)

    // Auto-onboarding: creates user if not found
    userID, isNew, err := s.store.ResolveUser(req.Channel, req.SenderID)
    if err != nil {
        return &protocol.Message{Type: protocol.TypeError, Error: err.Error()}
    }

    return &protocol.Message{
        Type: protocol.TypeUserResolved,
        Payload: mustMarshal(protocol.ResolveUserResponse{
            UserID: userID,
            IsNew:  isNew,
            Status: "active",
        }),
    }
}

func (s *Server) handlePollInbound(msg protocol.Message) *protocol.Message {
    var req protocol.PollInboundRequest
    json.Unmarshal(msg.Payload, &req)

    // Atomic claim: UPDATE ... SET status='claimed' WHERE status='pending' LIMIT 1
    inbound, err := s.store.ClaimInbound(req.UserID, req.AgentID)
    if err != nil || inbound == nil {
        return nil  // No message available
    }

    return &protocol.Message{
        Type:    protocol.TypeInbound,
        Payload: mustMarshal(inbound),
    }
}
```

### 3.2 Gateway Service (`internal/gateway`)

**Purpose**: Platform adapters with user resolution via bus.

```go
// gateway/gateway.go
package gateway

import (
    "context"

    "nanosec/internal/gateway/channel"
    "nanosec/internal/protocol"
)

type Gateway struct {
    busClient *BusClient
    channels  []channel.Channel
}

func New(busURL string, channels []channel.Channel, ackEnabled bool, ackText string) (*Gateway, error) {
    client, err := NewBusClient(busURL)
    if err != nil {
        return nil, err
    }

    return &Gateway{
        busClient: client,
        channels:  channels,
    }, nil
}

func (g *Gateway) Run(ctx context.Context) error {
    // Start outbound polling in background
    go g.pollOutbound(ctx)

    // Start all channel adapters
    for _, ch := range g.channels {
        ch := ch
        go func() {
            ch.Start(ctx, g.handleInbound)
        }()
    }

    <-ctx.Done()
    return ctx.Err()
}

func (g *Gateway) handleInbound(ctx context.Context, msg channel.InboundMessage) error {
    // 1. Resolve user via bus
    userID, err := g.busClient.ResolveUser(msg.Channel, msg.SenderID)
    if err != nil {
        slog.Warn("unknown user", "channel", msg.Channel, "sender", msg.SenderID)
        return nil  // Ignore unknown users
    }

    // 2. Post to bus
    return g.busClient.PostInbound(protocol.InboundMessage{
        UserID:    userID,
        SessionID: msg.Channel + ":" + msg.SenderID,
        Channel:   msg.Channel,
        ChatID:    msg.ChatID,
        SenderID:  msg.SenderID,
        Content:   msg.Content,
        Media:     msg.Media,
        Metadata:  msg.Metadata,
    })
}

func (g *Gateway) pollOutbound(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
            // Poll for each channel we handle
            for _, ch := range g.channels {
                msg, err := g.busClient.PollOutbound(ch.Name())
                if err != nil || msg == nil {
                    continue
                }

                ch.Send(ctx, channel.OutboundMessage{
                    ChatID:  msg.ChatID,
                    Content: msg.Content,
                    ReplyTo: msg.ReplyTo,
                    Media:   msg.Media,
                })
            }
            time.Sleep(100 * time.Millisecond)
        }
    }
}
```

#### Channel Adapter Interface

```go
// gateway/channel/channel.go
package channel

import "context"

type InboundMessage struct {
    Channel   string
    SenderID  string
    ChatID    string
    Content   string
    Media     []nanoprotocol.MediaItem
    Metadata  map[string]any
}

type OutboundMessage struct {
    ChatID  string
    Content string
    ReplyTo string
    Media   []nanoprotocol.MediaItem
}

type InboundHandler func(context.Context, InboundMessage) error

type Channel interface {
    Name() string
    Start(ctx context.Context, handler InboundHandler) error
    Send(ctx context.Context, msg OutboundMessage) error
    Stop() error
}
```

#### CLI Adapter (for development/testing)

```go
// gateway/channel/cli/adapter.go
package cli

import (
    "bufio"
    "context"
    "fmt"
    "os"

    "nanosec/internal/gateway/channel"
)

type Adapter struct {
    userID string  // Fixed user for CLI mode
    reader *bufio.Reader
}

func New(userID string) *Adapter {
    return &Adapter{
        userID: userID,
        reader: bufio.NewReader(os.Stdin),
    }
}

func (a *Adapter) Name() string { return "cli" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
    fmt.Println("CLI mode started. Type your messages (Ctrl+C to exit):")
    fmt.Print("> ")

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            line, err := a.reader.ReadString('\n')
            if err != nil {
                return err
            }

            if line == "" || line == "\n" {
                fmt.Print("> ")
                continue
            }

            handler(ctx, channel.InboundMessage{
                Channel:  "cli",
                SenderID: a.userID,
                ChatID:   "cli",
                Content:  line[:len(line)-1], // Remove newline
            })
        }
    }
}

func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
    fmt.Printf("\n%s\n> ", msg.Content)
    return nil
}

func (a *Adapter) Stop() error {
    return nil
}
```

#### WeCom (Enterprise WeChat) Adapter

```go
// gateway/channel/wecom/adapter.go
package wecom

import (
    "context"

    "github.com/xen0n/go-workwx"
    "nanosec/internal/gateway/channel"
)

type Adapter struct {
    client *workwx.Workwx
    agentID int64
}

func New(corpID, corpSecret string, agentID int64) (*Adapter, error) {
    client := workwx.New(corpID)
    app := client.WithApp(corpSecret, agentID)

    return &Adapter{
        client:  client,
        agentID: agentID,
    }, nil
}

func (a *Adapter) Name() string { return "wecom" }

func (a *Adapter) Start(ctx context.Context, handler channel.InboundHandler) error {
    // WeCom uses callback webhooks - this would be HTTP server based
    // Implementation depends on callback URL configuration
    // For polling mode, use message sync API
    return nil
}

func (a *Adapter) Send(ctx context.Context, msg channel.OutboundMessage) error {
    // Send text message to user
    return nil
}

func (a *Adapter) Stop() error {
    return nil
}
```

### 3.3 Agent Service (`internal/agent`)

**Purpose**: Per-user message processing with LLM and tools.

```go
// agent/agent.go
package agent

import (
    "context"

    "nanosec/internal/protocol"
    "nanosec/internal/provider"
    "nanosec/internal/session"
    "nanosec/internal/tool"
)

type Config struct {
    UserID        string
    BusURL        string
    Model         string
    MaxIterations int
    Workspace     string
    SessionDir    string
}

type Agent struct {
    config    Config
    busClient *BusClient
    provider  provider.Provider
    tools     *tool.Registry
    sessions  *session.Store
    instanceID string
}

func New(cfg Config, p provider.Provider, t *tool.Registry) (*Agent, error) {
    client, err := NewBusClient(cfg.BusURL)
    if err != nil {
        return nil, err
    }

    sessions, err := session.NewStore(cfg.SessionDir)
    if err != nil {
        return nil, err
    }

    return &Agent{
        config:     cfg,
        busClient:  client,
        provider:   p,
        tools:      t,
        sessions:   sessions,
        instanceID: uuid.New().String(),
    }, nil
}

func (a *Agent) Run(ctx context.Context) error {
    slog.Info("agent started", "user", a.config.UserID, "instance", a.instanceID)

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            // Poll for inbound message
            msg, err := a.busClient.PollInbound(a.config.UserID, a.instanceID)
            if err != nil {
                slog.Error("poll error", "error", err)
                time.Sleep(time.Second)
                continue
            }
            if msg == nil {
                time.Sleep(100 * time.Millisecond)
                continue
            }

            // Process message
            if err := a.processMessage(ctx, msg); err != nil {
                slog.Error("process error", "error", err)
            }
        }
    }
}
```

#### Message Processing Loop

```go
// agent/loop.go
package agent

import (
    "context"
    "fmt"

    "nanosec/internal/protocol"
    "nanosec/internal/provider"
)

func (a *Agent) processMessage(ctx context.Context, msg *protocol.InboundMessage) error {
    // 1. Get or create session (local JSONL file)
    sess := a.sessions.GetOrCreate(msg.SessionID)

    // 2. Add user message to session
    sess.AddMessage(provider.Message{
        Role:    "user",
        Content: msg.Content,
    })

    // 3. Build context
    messages := a.buildContext(sess)

    // 4. Get tool definitions
    tools := a.tools.Definitions()

    // 5. Iteration loop
    for i := 0; i < a.config.MaxIterations; i++ {
        resp, err := a.provider.Chat(ctx, provider.ChatRequest{
            Model:    a.config.Model,
            Messages: messages,
            Tools:    tools,
        })
        if err != nil {
            return fmt.Errorf("llm chat: %w", err)
        }

        // No tool calls = final response
        if len(resp.ToolCalls) == 0 {
            sess.AddMessage(provider.Message{
                Role:    "assistant",
                Content: resp.Content,
            })

            // Save session to file
            if err := a.sessions.Save(sess); err != nil {
                slog.Warn("save session failed", "error", err)
            }

            // Post response to bus
            return a.busClient.PostOutbound(protocol.OutboundMessage{
                UserID:    a.config.UserID,
                SessionID: msg.SessionID,
                Channel:   msg.Channel,
                ChatID:    msg.ChatID,
                Content:   resp.Content,
            })
        }

        // Execute tool calls
        messages = append(messages, provider.Message{
            Role:      "assistant",
            ToolCalls: resp.ToolCalls,
        })

        for _, tc := range resp.ToolCalls {
            result, err := a.tools.Execute(ctx, tc.Name, tc.Arguments)
            if err != nil {
                result = fmt.Sprintf("Error: %v", err)
            }

            messages = append(messages, provider.Message{
                Role:       "tool",
                ToolCallID: tc.ID,
                Content:    result,
            })
        }
    }

    return fmt.Errorf("max iterations (%d) reached", a.config.MaxIterations)
}
```

### 3.4 Session Storage (`internal/session`)

**Purpose**: File-based JSONL session storage (per agent).

```go
// session/store.go
package session

import (
    "bufio"
    "encoding/json"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "time"

    "nanosec/internal/provider"
)

type Session struct {
    ID        string             `json:"id"`
    Messages  []provider.Message `json:"messages"`
    CreatedAt time.Time          `json:"created_at"`
    UpdatedAt time.Time          `json:"updated_at"`
    mu        sync.RWMutex
}

func (s *Session) AddMessage(msg provider.Message) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.Messages = append(s.Messages, msg)
    s.UpdatedAt = time.Now()
}

type Store struct {
    dir   string
    cache map[string]*Session
    mu    sync.RWMutex
}

func NewStore(dir string) (*Store, error) {
    if err := os.MkdirAll(dir, 0755); err != nil {
        return nil, err
    }
    return &Store{
        dir:   dir,
        cache: make(map[string]*Session),
    }, nil
}

func (s *Store) GetOrCreate(sessionID string) *Session {
    s.mu.RLock()
    sess, ok := s.cache[sessionID]
    s.mu.RUnlock()
    if ok {
        return sess
    }

    // Try to load from file
    sess = s.load(sessionID)
    if sess == nil {
        sess = &Session{
            ID:        sessionID,
            Messages:  []provider.Message{},
            CreatedAt: time.Now(),
            UpdatedAt: time.Now(),
        }
    }

    s.mu.Lock()
    s.cache[sessionID] = sess
    s.mu.Unlock()

    return sess
}

func (s *Store) filePath(sessionID string) string {
    // "telegram:123" -> "telegram_123.jsonl"
    safe := strings.ReplaceAll(sessionID, ":", "_")
    return filepath.Join(s.dir, safe+".jsonl")
}

func (s *Store) load(sessionID string) *Session {
    path := s.filePath(sessionID)
    f, err := os.Open(path)
    if err != nil {
        return nil
    }
    defer f.Close()

    sess := &Session{ID: sessionID, Messages: []provider.Message{}}
    scanner := bufio.NewScanner(f)

    for scanner.Scan() {
        line := scanner.Text()
        if line == "" {
            continue
        }

        var data map[string]any
        if err := json.Unmarshal([]byte(line), &data); err != nil {
            continue
        }

        if data["_type"] == "metadata" {
            // Parse metadata line
            if t, ok := data["created_at"].(string); ok {
                sess.CreatedAt, _ = time.Parse(time.RFC3339, t)
            }
        } else {
            // Parse message line
            var msg provider.Message
            json.Unmarshal([]byte(line), &msg)
            sess.Messages = append(sess.Messages, msg)
        }
    }

    return sess
}

func (s *Store) Save(sess *Session) error {
    sess.mu.RLock()
    defer sess.mu.RUnlock()

    path := s.filePath(sess.ID)
    f, err := os.Create(path)
    if err != nil {
        return err
    }
    defer f.Close()

    // Write metadata line
    metadata := map[string]any{
        "_type":      "metadata",
        "created_at": sess.CreatedAt.Format(time.RFC3339),
        "updated_at": sess.UpdatedAt.Format(time.RFC3339),
    }
    json.NewEncoder(f).Encode(metadata)

    // Write message lines
    for _, msg := range sess.Messages {
        json.NewEncoder(f).Encode(msg)
    }

    return nil
}
```

### 3.5 WebSocket Protocol (`internal/protocol`)

**Purpose**: Shared message types for bus-gateway-agent communication.

```go
// protocol/messages.go
package protocol

import (
    "encoding/json"
    "time"
)

// Message types
const (
    // Gateway → Bus
    TypeResolveUser  = "resolve_user"
    TypePostInbound  = "post_inbound"
    TypePollOutbound = "poll_outbound"

    // Bus → Gateway
    TypeUserResolved = "user_resolved"
    TypeOutbound     = "outbound"

    // Agent → Bus
    TypePollInbound  = "poll_inbound"
    TypePostOutbound = "post_outbound"

    // Bus → Agent
    TypeInbound = "inbound"

    // Runner ↔ Bus (Phase 2)
    TypePostContainerRequest  = "post_container_request"
    TypePollContainerRequest  = "poll_container_request"
    TypeUpdateContainerStatus = "update_container_status"
    TypeUpdateUserStatus      = "update_user_status"

    // Bus → Runner (Phase 2)
    TypeContainerRequest = "container_request"

    // Bus → Gateway (Phase 2)
    TypeUserStatusUpdated = "user_status_updated"

    // Errors
    TypeError = "error"
)

// Message is the WebSocket message envelope
type Message struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
    Error   string          `json:"error,omitempty"`
}

// ResolveUserRequest - Gateway asks bus to resolve user
type ResolveUserRequest struct {
    Channel  string `json:"channel"`
    SenderID string `json:"sender_id"`
}

type ResolveUserResponse struct {
    UserID string `json:"user_id"`
    IsNew  bool   `json:"is_new"`
    Status string `json:"status"`
}

// InboundMessage - from gateway to bus to agent
type InboundMessage struct {
    UserID    string         `json:"user_id"`
    SessionID string         `json:"session_id"`  // "telegram:123456"
    Channel   string         `json:"channel"`
    ChatID    string         `json:"chat_id"`
    SenderID  string         `json:"sender_id"`
    Content   string         `json:"content"`
    Media     []MediaItem    `json:"media,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
    Timestamp time.Time      `json:"timestamp"`
}

// OutboundMessage - from agent to bus to gateway
type OutboundMessage struct {
    UserID    string         `json:"user_id"`
    SessionID string         `json:"session_id"`
    Channel   string         `json:"channel"`
    ChatID    string         `json:"chat_id"`
    Content   string         `json:"content"`
    Media     []MediaItem    `json:"media,omitempty"`
    ReplyTo   string         `json:"reply_to,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
}

// PollInboundRequest - Agent polls for messages
type PollInboundRequest struct {
    UserID  string `json:"user_id"`
    AgentID string `json:"agent_id"`  // Instance ID for claiming
}

// PollOutboundRequest - Gateway polls for messages
type PollOutboundRequest struct {
    Channel   string `json:"channel"`
    GatewayID string `json:"gateway_id"`
}

// MediaItem - media attachment in messages
type MediaItem struct {
    Type    string `json:"type"`             // "image", "video", "file", "voice"
    MediaID string `json:"media_id"`         // Platform media ID
    URL     string `json:"url,omitempty"`    // Direct URL if available
    Name    string `json:"name,omitempty"`   // File name
}

// ContainerRequest - Runner polls for these
type ContainerRequest struct {
    ID      string `json:"id"`
    UserID  string `json:"user_id"`
    Action  string `json:"action"`           // "provision" or "dispose"
    Image   string `json:"image,omitempty"`
}

// UpdateContainerStatusRequest - Runner posts status updates
type UpdateContainerStatusRequest struct {
    RequestID   string `json:"request_id"`
    Status      string `json:"status"`       // "done" or "failed"
    ContainerID string `json:"container_id,omitempty"`
    Error       string `json:"error,omitempty"`
}

// UpdateUserStatusRequest - update user status/level
type UpdateUserStatusRequest struct {
    UserID string `json:"user_id"`
    Status string `json:"status,omitempty"`
    Level  int    `json:"level,omitempty"`
}
```

### 3.6 Provider System (`internal/provider`)

**Purpose**: OpenAI-compatible LLM provider (unchanged from previous design).

```go
// provider/provider.go
package provider

import "context"

type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

type ChatRequest struct {
    Model    string    `json:"model"`
    Messages []Message `json:"messages"`
    Tools    []ToolDef `json:"tools,omitempty"`
}

type Message struct {
    Role       string     `json:"role"`
    Content    string     `json:"content,omitempty"`
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
    ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ChatResponse struct {
    Content   string     `json:"content"`
    ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}
```

### 3.7 Tool System (`internal/tool`)

**Purpose**: Agent capabilities (unchanged, but message tool uses bus client).

```go
// tool/message.go - Updated to use bus client
package tool

import (
    "context"
    "fmt"

    "nanosec/internal/protocol"
)

type MessageTool struct {
    busClient BusClient
    userID    string
}

type BusClient interface {
    PostOutbound(msg protocol.OutboundMessage) error
}

func NewMessageTool(client BusClient, userID string) *MessageTool {
    return &MessageTool{busClient: client, userID: userID}
}

func (m *MessageTool) Name() string { return "message" }

func (m *MessageTool) Description() string {
    return "Send a message to a chat channel"
}

func (m *MessageTool) Parameters() ParameterSchema {
    return ParameterSchema{
        Type: "object",
        Properties: map[string]PropertySchema{
            "channel": {Type: "string", Description: "Channel name (telegram, discord, etc.)"},
            "chat_id": {Type: "string", Description: "Chat ID to send to"},
            "content": {Type: "string", Description: "Message content"},
        },
        Required: []string{"channel", "chat_id", "content"},
    }
}

func (m *MessageTool) Execute(ctx context.Context, params map[string]any) (string, error) {
    channel := params["channel"].(string)
    chatID := params["chat_id"].(string)
    content := params["content"].(string)

    err := m.busClient.PostOutbound(protocol.OutboundMessage{
        UserID:    m.userID,
        SessionID: channel + ":" + chatID,
        Channel:   channel,
        ChatID:    chatID,
        Content:   content,
    })
    if err != nil {
        return "", fmt.Errorf("send message: %w", err)
    }

    return "Message sent successfully", nil
}
```

#### Media Tools (Phase 2)

```go
// tool/media_download.go - Download media from WeCom API
// Configured via AgentConfig.MediaConfig (CorpID, CorpSecret, AgentID)
func (m *MediaDownloadTool) Name() string { return "media_download" }
func (m *MediaDownloadTool) Description() string {
    return "Download a media file from WeCom by media_id; returns local file path"
}

// tool/media_upload.go - Upload media to WeCom API
func (m *MediaUploadTool) Name() string { return "media_upload" }
func (m *MediaUploadTool) Description() string {
    return "Upload a local file to WeCom media API; returns media_id"
}
```

### 3.8 Runner Service (`nanorunner/internal/runner`)

**Purpose**: Poll bus for pending container requests, provision/dispose Docker containers per user quota tier.

#### Poll-Provision-Update Lifecycle

```
Runner polls bus: {type: "poll_container_request"}
         │
         ▼
Bus returns: {type: "container_request", payload: {id, user_id, action, image}}
         │
         ▼
Runner checks quota tier (users.level):
  - Level 0: no container access
  - Level 1: single container, 512MB RAM
  - Level 2+: multiple containers, higher limits
         │
         ▼
Runner calls Docker SDK:
  docker.ContainerCreate / docker.ContainerRemove
         │
         ▼
Runner posts: {type: "update_container_status",
  payload: {request_id, status: "done", container_id}}
```

#### Quota Tiers

| Level | Max Containers | RAM Limit | CPU Quota |
|-------|---------------|-----------|-----------|
| 0 | 0 | — | — |
| 1 | 1 | 512MB | 0.5 CPU |
| 2 | 3 | 1GB | 1 CPU |
| 3+ | 10 | 4GB | 4 CPU |

#### Key Packages

- `internal/runner/runner.go` — poll loop, request dispatch
- `internal/runner/docker.go` — Docker SDK wrapper (github.com/docker/docker)
- `internal/runner/quotas.go` — tier enforcement, limit calculation

---

### 3.9 Terminal Sessions and the Path Contract

The hypervisor (`internal/hypervisor`) owns the interactive terminals shown in
the portal. Each terminal is an interactive `bash -i` under a PTY
(`internal/hypervisor/terminal.go`).

#### Two paths, deliberately different

| | Value | Why |
|---|---|---|
| **Working directory** (the shell's starting point) | the agent's own workspace, `<dataRoot>/agents/<homeName>` | a terminal belongs to an agent, so it starts where that agent works |
| **`HOME`** | the container user's home, `/home/<username>` | the terminal is the **user-level** channel; a person using it expects their own home |

Consequences worth knowing:

- `cd` with no argument, and `cd ~`, land in the **user home**, not in the
  agent workspace.
- `~/.bashrc`, `~/.profile`, `~/.ssh`, `~/.gitconfig` all resolve against the
  user home, because that is where the entrypoint seeds the profile files and
  where the user's own state lives.
- The agent process itself keeps a **different** `HOME`: `cmd/nanosec/main.go`
  points it at the agent root so per-agent tool state (opencode config and
  session data, caches) stays per-agent. The two values are intentionally
  separate and must not be "unified" — see the file header comment in
  `internal/hypervisor/terminal.go`.

#### How each half is resolved

- **Working directory**: `workspace.NewNamed(agentID, homeName(agentID), dataRoot).AgentWorkspace()`.
  The `homeName` resolver is wired from the agent registry so the folder matches
  the one the seeder assigned (see `agents/.homes.json`).
- **`HOME`**: `terminalHome()`, which reads the runtime identity's passwd entry
  via `os/user.Current()`. The binary is CGO-disabled, so this consults
  `/etc/passwd` directly and is immune to the process-global `HOME` override.
  It deliberately does **not** fall back to `os.UserHomeDir()` — that would
  return the agent root and quietly break the contract. If the passwd entry
  cannot be read it falls back to the entrypoint's naming rule
  (`/home/<AGENT_USERNAME>`) and logs a warning.

The environment is assembled by *replacement*, not by appending duplicates
(`withEnv` in `terminal.go`): consumers disagree on which of two entries with
the same key wins (glibc `getenv` takes the first, bash's environment import
keeps the last).

#### Coverage

`internal/hypervisor/terminal_test.go` pins both halves:
`TestTerminalOpenUsesUserHomeEnv` (HOME is the user home) and
`TestTerminalOpenStartsInAgentWorkspace` (`$PWD` is the agent workspace), plus
`TestTerminalHomeFallback` for the passwd-failure branch.

---

## 4. Data Flow Diagrams

### 4.1 Multi-User Message Flow

```
Alice sends "Hello" on Telegram (sender_id: 123456)
                │
                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ GATEWAY                                                                      │
│   1. Telegram adapter receives message                                       │
│   2. Send typing indicator                                                   │
│   3. WebSocket to Bus: {type: "resolve_user",                               │
│      payload: {channel: "telegram", sender_id: "123456"}}                   │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ BUS                                                                          │
│   4. Query: SELECT user_id FROM user_channels                               │
│      WHERE channel='telegram' AND sender_id='123456'                        │
│   5. Response: {type: "user_resolved", payload: {user_id: "alice"}}         │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ GATEWAY                                                                      │
│   6. WebSocket to Bus: {type: "post_inbound", payload: {                    │
│      user_id: "alice", session_id: "telegram:123456",                       │
│      channel: "telegram", chat_id: "123456", content: "Hello"}}             │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ BUS                                                                          │
│   7. INSERT INTO inbound_queue (user_id, session_id, ...) VALUES (...)      │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ ALICE'S AGENT (polling)                                                      │
│   8. WebSocket to Bus: {type: "poll_inbound",                               │
│      payload: {user_id: "alice", agent_id: "agent-uuid"}}                   │
│   9. Bus claims message: UPDATE inbound_queue SET status='claimed'          │
│  10. Bus returns: {type: "inbound", payload: {...}}                         │
│  11. Load session from ~/.nanosec/sessions/telegram_123456.jsonl            │
│  12. Add user message to session                                            │
│  13. Build context, call LLM                                                │
│  14. Execute tools if needed (loop)                                         │
│  15. Get final response                                                     │
│  16. Save session to file                                                   │
│  17. WebSocket to Bus: {type: "post_outbound", payload: {                   │
│      user_id: "alice", session_id: "telegram:123456",                       │
│      channel: "telegram", chat_id: "123456",                                │
│      content: "Hi! How can I help?"}}                                       │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ BUS                                                                          │
│  18. INSERT INTO outbound_queue (user_id, channel, ...) VALUES (...)        │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ GATEWAY (polling outbound for telegram)                                      │
│  19. WebSocket to Bus: {type: "poll_outbound",                              │
│      payload: {channel: "telegram", gateway_id: "gw-uuid"}}                 │
│  20. Bus claims message, returns: {type: "outbound", payload: {...}}        │
│  21. Telegram adapter sends to Telegram API                                 │
└─────────────────────────────────────────────────────────────────────────────┘
                │
                ▼
         Alice sees "Hi! How can I help?"
```

### 4.2 Tool Execution Flow

```
┌──────────────────────────────────────────────────────────────────────────┐
│                         Tool Execution Flow                               │
└──────────────────────────────────────────────────────────────────────────┘

  LLM Response (within Agent)
       │
       │ ToolCalls: [{id: "1", name: "exec", args: {command: "ls"}}]
       ▼
┌──────────────────────────────────────┐
│           Tool Registry              │
│  tools.Execute(ctx, "exec", args)    │
└──────────────────┬───────────────────┘
                   │
                   ▼
┌──────────────────────────────────────┐
│          Execute Tool                │
│  result, err := tool.Execute(...)    │
└──────────────────┬───────────────────┘
                   │
                   ▼
┌──────────────────────────────────────┐
│        Append to Messages            │
│  {role: "tool", tool_call_id: "1",   │
│   content: "file1 file2"}            │
└──────────────────┬───────────────────┘
                   │
                   ▼
           Loop back to LLM
```

### 4.3 Auto-Onboarding Flow

```
New user sends first message (no prior user_channels entry)
                │
                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ GATEWAY                                                                      │
│   1. Adapter receives message                                                │
│   2. Send to Bus: {type: "resolve_user",                                    │
│      payload: {channel: "wechatkf", sender_id: "oXxxx"}}                   │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ BUS                                                                          │
│   3. No user_channels match found                                           │
│   4. Auto-onboard: INSERT INTO users (id) VALUES (uuid())                  │
│      INSERT INTO user_channels (user_id, channel, sender_id) ...           │
│   5. Response: {type: "user_resolved",                                      │
│      payload: {user_id: "new-uuid", is_new: true, status: "active"}}       │
└─────────────────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
                          Gateway continues posting inbound message
```

### 4.4 Media Message Flow

```
User sends image on WeChat KF
                │
                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ GATEWAY (WeChat KF adapter)                                                  │
│   1. Adapter polls WeChat KF sync API, receives message with media_id       │
│   2. Posts to Bus with Media: [{type:"image", media_id:"xxx"}]              │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │ WebSocket
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ BUS                                                                          │
│   3. Stores InboundMessage with media JSON in inbound_queue                 │
└───────────────────────────────────────┬─────────────────────────────────────┘
                                        │
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ AGENT                                                                        │
│   4. Polls inbound, receives message with Media field                       │
│   5. Calls media_download tool with media_id                                │
│   6. Processes image (passes to LLM as base64 or file URL)                 │
│   7. Posts outbound response (may include media_id if uploading back)       │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 4.5 Scaling Diagram

```
                         Load Balancer
                              │
              ┌───────────────┼───────────────┐
              │               │               │
              ▼               ▼               ▼
         ┌─────────┐     ┌─────────┐     ┌─────────┐
         │  Bus 1  │     │  Bus 2  │     │  Bus 3  │
         └────┬────┘     └────┬────┘     └────┬────┘
              │               │               │
              └───────────────┼───────────────┘
                              │
                              ▼
                    ┌─────────────────┐
                    │     MongoDB     │
                    │   (Shared DB)   │
                    └─────────────────┘
                              ▲
              ┌───────────────┼───────────────┐
              │               │               │
         ┌────┴────┐     ┌────┴────┐     ┌────┴────┐
         │Gateway 1│     │Gateway 2│     │Gateway 3│
         │  CLI    │     │  WeCom  │     │ Feishu  │
         └─────────┘     └─────────┘     └─────────┘

              ┌───────────────┼───────────────┐
              │               │               │
         ┌────┴────┐     ┌────┴────┐     ┌────┴────┐
         │ Agent   │     │ Agent   │     │ Agent   │
         │ alice   │     │  bob    │     │ carol   │
         └─────────┘     └─────────┘     └─────────┘
```

---

## 5. Error Handling Strategy

### 5.1 Principles

1. **Explicit Returns**: All functions return `(result, error)` - no panics except for unrecoverable bugs
2. **Error Wrapping**: Use `fmt.Errorf("context: %w", err)` for stack traces
3. **Error Matching**: Use `errors.Is()` and `errors.As()` for specific error handling
4. **Sentinel Errors**: Define package-level errors for common cases

### 5.2 Error Types

```go
// errors.go (per package)
package agent

import "errors"

var (
    ErrMaxIterations = errors.New("max iterations reached")
    ErrToolNotFound  = errors.New("tool not found")
    ErrInvalidParams = errors.New("invalid parameters")
)
```

### 5.3 Error Handling Patterns

```go
// Wrapping errors with context
result, err := tool.Execute(ctx, params)
if err != nil {
    return fmt.Errorf("execute tool %s: %w", name, err)
}

// Checking specific errors
if errors.Is(err, ErrToolNotFound) {
    // Handle missing tool
}

// Logging errors
if err != nil {
    slog.Error("tool execution failed",
        "tool", name,
        "error", err,
    )
}
```

### 5.4 Panic Recovery

```go
// Recover from panics in goroutines
func (a *Agent) processMessage(ctx context.Context, msg bus.InboundMessage) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("panic: %v", r)
            slog.Error("panic in message processing", "panic", r)
        }
    }()

    // ... processing logic
}
```

---

## 6. Configuration Management

### 6.1 Configuration Files

Each service has its own configuration file:

| Service | Location | Description |
|---------|----------|-------------|
| Bus | `~/.nanosec/bus.json` | MongoDB URI, WebSocket port |
| Gateway | `~/.nanosec/gateway.json` | Bus URL, channel credentials |
| Agent | `~/.nanosec/agent.json` | Bus URL, provider config, tools |

### 6.2 Bus Configuration

```json
{
  "server": {
    "port": 9000,
    "host": "0.0.0.0"
  },
  "database": {
    "uri": "mongodb://user:password@localhost:27017/nanobus?authSource=admin"
  }
}
```

### 6.3 Gateway Configuration

```json
{
  "bus": {
    "url": "ws://localhost:9000"
  },
  "channels": {
    "cli": {
      "enabled": true,
      "userID": "alice"
    },
    "wecom": {
      "enabled": false,
      "corpID": "",
      "corpSecret": "",
      "agentID": 0
    },
    "feishu": {
      "enabled": false,
      "appId": "",
      "appSecret": ""
    }
  }
}
```

### 6.4 Agent Configuration

```json
{
  "bus": {
    "url": "ws://localhost:9000"
  },
  "provider": {
    "apiKey": "sk-...",
    "baseUrl": "https://api.openai.com/v1",
    "defaultModel": "gpt-4o",
    "subagentModel": "gpt-4o-mini"
  },
  "agent": {
    "maxIterations": 20
  },
  "tools": {
    "restrictToWorkspace": true,
    "shell": {
      "timeout": "60s"
    }
  },
  "paths": {
    "dataRoot": "~/.nanosec",
    "sessionDir": "~/.nanosec/sessions",
    "cronFile": "~/.nanosec/cron.json",
    "mediaBaseDir": "~/.nanosec/media",
    "workspaceDir": "/workspace",
    "identityFile": "/workspace/AGENT.md",
    "bootstrapDir": "/workspace/bootstrap",
    "memoryDir": "/workspace/memory",
    "skillsDir": "/workspace/skills"
  }
}
```

### 6.5 Environment Variable Overrides

```bash
# Bus
export NANOSEC_BUS_SERVER_PORT=9000
export NANOSEC_BUS_DATABASE_URI="mongodb://user:pass@localhost:27017/nanobus?authSource=admin"

# Gateway
export NANOSEC_GATEWAY_BUS_URL="ws://localhost:9000"
export NANOSEC_GATEWAY_CHANNELS_WECOM_CORPID="your-corp-id"

# Agent
export NANOSEC_AGENT_BUS_URL="ws://localhost:9000"
export NANOSEC_AGENT_PROVIDER_APIKEY="sk-..."
export NANOSEC_AGENT_PROVIDER_BASEURL="https://api.openai.com/v1"
```

---

## 7. Logging

### 7.1 Structured Logging with slog

```go
// main.go
import "log/slog"

func main() {
    // Configure structured logging
    logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
        Level: slog.LevelInfo,
    }))
    slog.SetDefault(logger)

    // Usage in code
    slog.Info("agent started", "model", cfg.Model)
    slog.Error("tool failed", "tool", name, "error", err)
}
```

### 7.2 Log Levels

| Level | Usage |
|-------|-------|
| Debug | Internal state, message contents |
| Info | Startup, connections, high-level flow |
| Warn | Recoverable issues, retries |
| Error | Failures that affect functionality |

---

## 8. Testing Strategy

See [TESTING.md](./TESTING.md) for detailed testing strategy.

---

## 9. Deployment

See [DEPLOYMENT.md](./DEPLOYMENT.md) for deployment guide.

---

## 10. Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
| 2.0 | 2026-02-09 | Claude | Multi-user decoupled architecture (Gateway, Bus, Agent) |
