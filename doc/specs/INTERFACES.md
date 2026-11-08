# Interface Contracts

**Version**: 2.0
**Date**: 2026-07-30
**Status**: Current

This document defines the core interfaces and data contracts for the MyriadMind system across all modules.

---

## 1. nanoprotocol (shared message types)

### 1.1 Message Envelope

```go
type Message struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
    Error   string          `json:"error,omitempty"`
}
```

### 1.2 Type Constants

```
// Gateway -> Bus
TypeResolveUser  = "resolve_user"
TypePostInbound  = "post_inbound"
TypePollOutbound = "poll_outbound"

// Bus -> Gateway
TypeUserResolved = "user_resolved"
TypeOutbound     = "outbound"

// Agent -> Bus
TypePollInbound  = "poll_inbound"
TypePostOutbound = "post_outbound"

// Bus -> Agent
TypeInbound = "inbound"

// Runner <-> Bus
TypePollPendingContainers = "poll_pending_containers"
TypePendingContainers     = "pending_containers"
TypeUpdateContainerStatus = "update_container_status"

// User management
TypeUpdateUserStatus = "update_user_status"
TypeGetUserInfo      = "get_user_info"
TypeUserInfo         = "user_info"

// WeChat KF sync
TypeGetWeChatKFSyncState      = "get_wechatkf_sync_state"
TypeWeChatKFSyncState         = "wechatkf_sync_state"
TypeUpdateWeChatKFCursor      = "update_wechatkf_cursor"
TypeUpdateWeChatKFCallbackTok = "update_wechatkf_callback_token"

// Configuration
TypeGetConfigItems = "get_config_items"
TypeConfigItems    = "config_items"
TypeSetConfigItem  = "set_config_item"
TypeConfigItemSet  = "config_item_set"

// Runner registry
TypeRegisterRunner   = "register_runner"
TypeRunnerRegistered = "runner_registered"

// Error
TypeError = "error"
```

### 1.3 Client

```go
type Client struct { /* unexported */ }

func NewClient(url string) (*Client, error)
func (c *Client) Close() error
func (c *Client) Send(msg Message) (*Message, error)

// Typed RPC methods (one per protocol type)
func (c *Client) ResolveUser(channel, senderID string) (*ResolveUserResponse, error)
func (c *Client) PostInbound(inbound InboundMessage) error
func (c *Client) PollInbound(userID string, bindingID int64, agentID string) (*InboundMessage, error)
func (c *Client) PostOutbound(outbound OutboundMessage) error
func (c *Client) PollOutbound(channel, gatewayID string) (*OutboundMessage, error)
func (c *Client) PollPendingContainers(runnerID string) ([]ContainerRequest, error)
func (c *Client) UpdateContainerStatus(userID string, bindingID int64, status string) error
func (c *Client) UpdateUserStatus(userID, status string) error
func (c *Client) GetUserInfo(userID string) (*UserInfoResponse, error)
func (c *Client) GetWeChatKFSyncState(openKfID string) (*WeChatKFSyncStateResponse, error)
func (c *Client) UpdateWeChatKFCursor(openKfID, cursor string) error
func (c *Client) UpdateWeChatKFCallbackToken(openKfID, callbackToken string, tokenExpiresAt time.Time) error
func (c *Client) GetConfigItems(componentType string) ([]ConfigItem, error)
func (c *Client) SetConfigItem(componentType, key, value string) error
func (c *Client) RegisterRunner(runnerID, hostname, dockerHost string) (*RegisterRunnerResponse, error)
```

---

## 2. nanobus (bus service)

### 2.1 Store

```go
type Store struct { /* db *sql.DB */ }

func Open(dsn string) (*Store, error)
func New(db *sql.DB) *Store
func (s *Store) Close() error
func (s *Store) DB() *sql.DB

// Users
func (s *Store) RegisterUser(userID string) error
func (s *Store) LinkChannel(userID, channel, senderID string) error
func (s *Store) ResolveUser(channel, senderID string) (userID string, isNew bool, err error)
func (s *Store) ResolveUserBinding(channel, senderID string) (ResolvedBinding, error)
func (s *Store) ListUsers() ([]string, error)
func (s *Store) UpdateUserStatus(userID, status string) error
func (s *Store) GetUserInfo(userID string) (status, level string, err error)
func (s *Store) EnableChannel(userID, channel string) error
func (s *Store) DisableChannel(userID, channel string) error

// Queues
func (s *Store) PostInbound(msg nanoprotocol.InboundMessage) error
func (s *Store) ClaimInbound(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error)
func (s *Store) PostOutbound(msg nanoprotocol.OutboundMessage) error
func (s *Store) ClaimOutbound(channel, gatewayID string) (*nanoprotocol.OutboundMessage, error)

// Containers
func (s *Store) PollPendingContainers(runnerID string) ([]nanoprotocol.ContainerRequest, error)
func (s *Store) UpdateContainerStatus(userID string, bindingID int64, status string) error

// Runners
func (s *Store) RegisterRunner(id, hostname, dockerHost string) error
func (s *Store) UpdateRunnerHeartbeat(id string) error

// Config
func (s *Store) GetConfigItems(componentType string) ([]ConfigItem, error)
func (s *Store) GetConfigItem(componentType, key string) (ConfigItem, error)
func (s *Store) SetConfigItem(componentType, key, value string) error
func (s *Store) SeedConfigItems(componentType string, items []ConfigItem) error

// WeChat KF
func (s *Store) GetWeChatKFSyncState(openKfID string) (*nanoprotocol.WeChatKFSyncStateResponse, error)
func (s *Store) UpdateWeChatKFCursor(openKfID, cursor string) error
func (s *Store) UpdateWeChatKFCallbackToken(openKfID, callbackToken string, tokenExpiresAt time.Time) error

// Migrations
func (s *Store) Migrate(migrations map[string]string) ([]string, error)
func (s *Store) MigrationStatus(migrations map[string]string) ([]MigrationInfo, error)
```

### 2.2 Server

```go
type Server struct { /* store, upgrader, listener, srv, mu */ }

func NewServer(s *store.Store) *Server
func (s *Server) ListenAndServe(addr string) error
func (s *Server) Addr() string
func (s *Server) Close() error
```

Server handles all protocol message types in `handler.go` via a type switch on `msg.Type`.

### 2.3 Configuration

```go
type BusConfig struct {
    Server   ServerConfig
    Database DatabaseConfig
}
type ServerConfig struct { Host string; Port int }
type DatabaseConfig struct { Driver string; DSN string }

func DefaultBusConfig() BusConfig
func LoadBusConfig(path string) (BusConfig, error)
func ValidateBusConfig(cfg BusConfig) error
```

---

## 3. nanogateway (gateway service)

### 3.1 Channel Interface

```go
type InboundMessage struct {
    Channel   string
    SenderID  string
    ChatID    string
    MessageID string
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

**Implementation Requirements:**
- `Name()` must return a stable identifier: `"cli"`, `"wecom"`, `"feishu"`, `"wechatkf"`
- `Start()` must block until context is cancelled
- `Start()` must call `handler` for each inbound message received
- `Send()` must deliver the message to the platform
- `Stop()` must complete within 5 seconds

### 3.2 Channel Implementations

| Adapter | Package | Protocol | Key Dependency |
|---------|---------|----------|---------------|
| CLI | `internal/channel/cli` | stdin/stdout | stdlib `bufio` |
| WeCom | `internal/channel/wecom` | HTTP callback | go-workwx/v2 |
| Feishu | `internal/channel/feishu` | WebSocket | oapi-sdk-go/v3 |
| WeChat KF | `internal/channel/wechatkf` | Polling + HTTP callback | go-workwx/v2, AES-256-CBC |

### 3.3 BusClient

```go
type BusClient struct { /* wraps nanoprotocol.Client */ }

func NewBusClient(url string) (*BusClient, error)
func (b *BusClient) ResolveUser(channel, senderID string) (*nanoprotocol.ResolveUserResponse, error)
func (b *BusClient) PostInbound(msg nanoprotocol.InboundMessage) error
func (b *BusClient) PollOutbound(channel, gatewayID string) (*nanoprotocol.OutboundMessage, error)
func (b *BusClient) Close() error
```

### 3.4 Gateway

```go
type Gateway struct { /* busClient, channels, gatewayID, ackEnabled, ackText */ }

func New(busURL string, channels []Channel, ackEnabled bool, ackText string) (*Gateway, error)
func (g *Gateway) Run(ctx context.Context) error
```

### 3.5 Configuration

```go
type GatewayConfig struct {
    Channels ChannelsConfig
    Ack      AckConfig
}
type AckConfig struct { Enabled bool; Text string }
type ChannelsConfig struct {
    CLI      CLIChannelConfig
    WeCom    WeComChannelConfig
    Feishu   FeishuChannelConfig
    WeChatKF WeChatKFChannelConfig
}
```

---

## 4. nanosec (agent service)

### 4.1 BusClient Interface

```go
type BusClient interface {
    PollInbound(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error)
    PostOutbound(nanoprotocol.OutboundMessage) error
    Close() error
}
```

### 4.2 Agent

```go
type Config struct {
    UserID, BindingID, BusURL, Model string
    MaxIterations int
    SessionDir    string
    ContextWindow, MaxOutputTokens, MaxToolResultChars int
    SummarizeTokenPercent, SummarizeMessageThreshold   int
    IdleSessionTimeout time.Duration
}

type Agent struct { /* unexported */ }

func New(cfg Config, client BusClient, p provider.Provider, t *tool.Registry) (*Agent, error)
func (a *Agent) SetContextBuilder(cb *ContextBuilder)
func (a *Agent) Run(ctx context.Context) error
```

### 4.3 Provider Interface

```go
type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

type ChatRequest struct {
    Model    string
    Messages []Message
    Tools    []ToolDef
}

type Message struct {
    Role       string
    Content    string
    ToolCalls  []ToolCall
    ToolCallID string
}

type ToolCall struct {
    ID       string
    Type     string
    Function ToolCallFunction
}
type ToolCallFunction struct { Name string; Arguments string }

type ToolDef struct {
    Type     string
    Function FunctionDef
}
type FunctionDef struct { Name string; Description string; Parameters json.RawMessage }

type ChatResponse struct {
    Content   string
    ToolCalls []ToolCall
    Usage     *Usage
}
type Usage struct { PromptTokens int; CompletionTokens int; TotalTokens int }

// Implementation
type OpenAIProvider struct { /* wraps go-openai */ }
func NewOpenAI(apiKey, baseURL, defaultModel string) *OpenAIProvider

// Mock for testing
type MockProvider struct { /* responses queue */ }
func NewMock() *MockProvider
```

### 4.4 Tool Interface

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() ParameterSchema
    Execute(ctx context.Context, params map[string]any) (string, error)
}

type ContextualTool interface {
    Tool
    SetContext(channel, chatID string)
}

type ParameterSchema struct {
    Type       string
    Properties map[string]PropertySchema
    Required   []string
}
type PropertySchema struct {
    Type        string
    Description string
    Enum        []string
    Default     any
}

type Registry struct { /* tools map */ }
func NewRegistry() *Registry
func (r *Registry) Register(t Tool)
func (r *Registry) Get(name string) (Tool, bool)
func (r *Registry) Execute(ctx context.Context, name string, argumentsJSON string) (string, error)
func (r *Registry) Definitions() []ToolDef
func (r *Registry) SetContext(channel, chatID string)
```

**Tools that implement ContextualTool:** `SendMessage`, `CronTool`, `SpawnTool`, `MediaUploadTool`

### 4.5 Session Interface

```go
type Session struct { ID string; Messages []provider.Message; ... }

func New(id string) *Session
func (s *Session) AddMessage(msg provider.Message)
func (s *Session) GetMessages() []provider.Message
func (s *Session) MessageCount() int
func (s *Session) SetSummary(summary string)
func (s *Session) GetSummary() string
func (s *Session) SetMeta(key, value string)
func (s *Session) GetMeta(key string) (string, bool)
func (s *Session) GetAllMeta() map[string]string
func (s *Session) TruncateMessages(keepLast int)
func (s *Session) Clear()

type Store struct { /* dir + cache map */ }
func NewStore(dir string) (*Store, error)
func (s *Store) GetOrCreate(sessionID string) *Session
func (s *Store) ActiveSession(baseKey string) (*Session, error)
func (s *Store) NewSession(baseKey string) (*Session, error)
func (s *Store) ArchiveSession(sessionID string) error
func (s *Store) Save(sess *Session) error

func IsIdle(sess *Session, timeout time.Duration) bool
func ParseSessionKey(id string) (channel, chatID, version string)
```

### 4.6 Context Builder

```go
type ContextBuilder struct { /* workspaceDir, identityFile, bootstrapDir, memoryDir, skills */ }

func NewContextBuilder(workspaceDir, identityFile, bootstrapDir, memoryDir string, skills *skill.Registry, memoryRecentDays int) *ContextBuilder
func (cb *ContextBuilder) BuildSystemPrompt(channel, chatID string) string
```

### 4.7 Skill System

```go
type Skill struct {
    Name        string
    Description string
    Content     string
    Always      bool
}

type Loader struct { /* skillsDir */ }
func NewLoader(skillsDir string) *Loader
func (l *Loader) List() ([]Skill, error)
func (l *Loader) Load(name string) (*Skill, error)
func (l *Loader) LoadForContext(names []string) (string, error)
func (l *Loader) BuildSummary() string
func (l *Loader) GetAlwaysSkills() []string

type Registry struct { /* wraps Loader with cache */ }
func NewRegistry(loader *Loader) *Registry
func (r *Registry) List() []Skill
func (r *Registry) Get(name string) (*Skill, error)
func (r *Registry) BuildSummary() string
func (r *Registry) GetAlwaysSkills() []string
func (r *Registry) LoadForContext(names []string) (string, error)
```

### 4.8 Cron Service

```go
type ScheduleKind string
const (
    ScheduleKindAt    ScheduleKind = "at"
    ScheduleKindEvery ScheduleKind = "every"
    ScheduleKindCron  ScheduleKind = "cron"
)

type Schedule struct { Kind ScheduleKind; Value string }
type Payload struct { Message string }
type JobState struct { Schedule Schedule; Payload Payload }
type Job struct { ID string; Name string; Schedule Schedule; Payload Payload; ... }

type JobCallback func(ctx context.Context, job *Job)

type Service struct { /* robfig/cron/v3 scheduler + JSON persistence */ }
func New(storePath string, onJob JobCallback) *Service
func (s *Service) Start(ctx context.Context)
func (s *Service) Stop()
func (s *Service) AddJob(name string, sched Schedule, payload Payload, deleteAfterRun bool) (string, error)
func (s *Service) RemoveJob(id string) error
func (s *Service) ListJobs() []Job
```

### 4.9 Subagent Manager

```go
type SubagentConfig struct {
    Model         string
    MaxIterations int
    Workspace     string
    ShellTimeout  time.Duration
    SearchAPIKey  string
}

type SubagentManager struct { /* config, provider, busClient, userID */ }
func NewSubagentManager(cfg SubagentConfig, prov provider.Provider, busClient BusClient, userID string) *SubagentManager
func (m *SubagentManager) Spawn(ctx context.Context, task, label, originChannel, originChatID string) string
```

### 4.10 Configuration

```go
type AgentConfig struct {
    Provider ProviderConfig
    Agent    AgentCoreConfig
    Tools    ToolsConfig
    Media    MediaConfig
    Paths    PathsConfig
}
type ProviderConfig struct { APIKey string; BaseURL string; DefaultModel string }
type AgentCoreConfig struct { MaxIterations int; SessionDir string; ContextWindow int; MaxOutputTokens int; MaxToolResultChars int; SummarizeTokenPercent int; SummarizeMessageThreshold int; IdleSessionTimeout time.Duration }
type ToolsConfig struct { Workspace string; RestrictToWorkspace bool; Shell ShellConfig; Web WebConfig }
type ShellConfig struct { Timeout time.Duration }
type WebConfig struct { SearchAPIKey string }
type MediaConfig struct { CorpID string; CorpSecret string; AgentID int64 }
type PathsConfig struct { Workspace string; IdentityFile string; BootstrapDir string; MemoryDir string; SkillsDir string; SessionDir string; CronStorePath string }
```

---

## 5. nanorunner (runner service)

### 5.1 DockerClient Interface

```go
type DockerClient interface {
    CreateContainer(ctx context.Context, req nanoprotocol.ContainerRequest, image, busURL, network string, quota Quota) (string, error)
    StartContainer(ctx context.Context, containerID string) error
    StopContainer(ctx context.Context, containerID string) error
    RemoveContainer(ctx context.Context, containerID string) error
}

type RealDockerClient struct { cli *dockerclient.Client }
func NewDockerClient() (*RealDockerClient, error)
```

### 5.2 Runner

```go
type Runner struct { /* config, busClient, dockerClient, containers map */ }

func New(cfg *config.RunnerConfig, busClient *nanoprotocol.Client, docker DockerClient) *Runner
func (r *Runner) Run(ctx context.Context) error
func (r *Runner) DisposeContainer(ctx context.Context, userID string, bindingID int64) error
```

### 5.3 Quotas

```go
type Quota struct { MemoryMB int64; CPUs int64 }

func QuotaForLevel(level string) Quota
// free:  512MB / 1 CPU
// pro:   2048MB / 2 CPU
// ultra: 4096MB / 4 CPU
```

### 5.4 Configuration

```go
type RunnerConfig struct {
    Runner     RunnerSettings
    AgentImage string
    BusURL     string
    Network    string
}
type RunnerSettings struct { PollInterval int; RunnerID string }
```

---

## 6. Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft (single-process architecture) |
| 2.0 | 2026-07-30 | Claude | Full rewrite for decoupled 5-module architecture; documents real interfaces from all modules; added ContextualTool, DockerClient, SubagentManager, ConfigItems API |
