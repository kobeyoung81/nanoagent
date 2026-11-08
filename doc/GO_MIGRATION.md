# Go Migration Guide: Python to Go Pattern Mapping

**Version**: 1.0
**Date**: 2026-02-08
**Status**: Draft

This document provides a comprehensive guide for translating Python nanobot patterns to idiomatic Go.

---

## 1. Concurrency: asyncio → Goroutines + Channels

### 1.1 Async Functions

**Python:**
```python
async def process_message(self, msg: InboundMessage) -> str:
    result = await self.llm.chat(messages)
    return result.content
```

**Go:**
```go
func (a *Agent) processMessage(ctx context.Context, msg bus.InboundMessage) (string, error) {
    result, err := a.llm.Chat(ctx, messages)
    if err != nil {
        return "", fmt.Errorf("llm chat: %w", err)
    }
    return result.Content, nil
}
```

**Key Differences:**
- Go functions are synchronous by default; use goroutines for concurrency
- `context.Context` replaces asyncio's implicit cancellation
- Explicit error returns instead of exceptions

### 1.2 Async Queues → Buffered Channels

**Python:**
```python
class MessageBus:
    def __init__(self):
        self.inbound: asyncio.Queue[InboundMessage] = asyncio.Queue()
        self.outbound: asyncio.Queue[OutboundMessage] = asyncio.Queue()

    async def publish_inbound(self, msg: InboundMessage) -> None:
        await self.inbound.put(msg)

    async def consume_inbound(self) -> InboundMessage:
        return await self.inbound.get()
```

**Go:**
```go
type Bus struct {
    inbound  chan InboundMessage
    outbound chan OutboundMessage
}

func NewBus(size int) *Bus {
    return &Bus{
        inbound:  make(chan InboundMessage, size),
        outbound: make(chan OutboundMessage, size),
    }
}

func (b *Bus) PublishInbound(ctx context.Context, msg InboundMessage) error {
    select {
    case <-ctx.Done():
        return ctx.Err()
    case b.inbound <- msg:
        return nil
    }
}

func (b *Bus) Inbound() <-chan InboundMessage {
    return b.inbound
}
```

**Key Differences:**
- Buffered channels replace asyncio.Queue
- Channel send (`<-`) is blocking; use `select` for non-blocking
- Return read-only channel (`<-chan`) for consumers

### 1.3 asyncio.gather → errgroup

**Python:**
```python
async def start_all_channels(self):
    await asyncio.gather(
        self.telegram.start(),
        self.discord.start(),
        self.feishu.start(),
    )
```

**Go:**
```go
import "golang.org/x/sync/errgroup"

func (m *Manager) StartAll(ctx context.Context) error {
    g, ctx := errgroup.WithContext(ctx)

    g.Go(func() error {
        return m.telegram.Start(ctx)
    })
    g.Go(func() error {
        return m.discord.Start(ctx)
    })
    g.Go(func() error {
        return m.feishu.Start(ctx)
    })

    return g.Wait()
}
```

**Key Differences:**
- `errgroup` provides structured concurrency with error propagation
- Context cancellation propagates to all goroutines
- First error cancels other goroutines

### 1.4 asyncio.create_task → go func()

**Python:**
```python
async def spawn_subagent(self, task: str):
    asyncio.create_task(self._run_subagent(task))
    return "Subagent spawned"
```

**Go:**
```go
func (a *Agent) spawnSubagent(ctx context.Context, task string) string {
    go func() {
        if err := a.runSubagent(ctx, task); err != nil {
            slog.Error("subagent failed", "error", err)
        }
    }()
    return "Subagent spawned"
}
```

**Key Differences:**
- `go` keyword spawns a goroutine
- No task handle returned; use channels for results
- Must handle panics in goroutines

### 1.5 asyncio.wait_for → context.WithTimeout

**Python:**
```python
async def execute_with_timeout(self, cmd: str) -> str:
    try:
        result = await asyncio.wait_for(
            self._execute(cmd),
            timeout=60.0
        )
        return result
    except asyncio.TimeoutError:
        return "Command timed out"
```

**Go:**
```go
func (s *ShellTool) executeWithTimeout(ctx context.Context, cmd string) (string, error) {
    ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
    defer cancel()

    result, err := s.execute(ctx, cmd)
    if errors.Is(err, context.DeadlineExceeded) {
        return "", fmt.Errorf("command timed out")
    }
    return result, err
}
```

### 1.6 asyncio.Event → chan struct{}

**Python:**
```python
class Service:
    def __init__(self):
        self._shutdown = asyncio.Event()

    async def run(self):
        while not self._shutdown.is_set():
            await self.process()

    def stop(self):
        self._shutdown.set()
```

**Go:**
```go
type Service struct {
    shutdown chan struct{}
}

func NewService() *Service {
    return &Service{
        shutdown: make(chan struct{}),
    }
}

func (s *Service) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-s.shutdown:
            return nil
        default:
            if err := s.process(ctx); err != nil {
                return err
            }
        }
    }
}

func (s *Service) Stop() {
    close(s.shutdown)
}
```

---

## 2. Type System: Duck Typing → Explicit Interfaces

### 2.1 Abstract Base Classes → Interfaces

**Python:**
```python
from abc import ABC, abstractmethod

class Tool(ABC):
    @abstractmethod
    def name(self) -> str:
        pass

    @abstractmethod
    def description(self) -> str:
        pass

    @abstractmethod
    async def execute(self, **kwargs) -> str:
        pass

class ShellTool(Tool):
    def name(self) -> str:
        return "exec"

    def description(self) -> str:
        return "Execute shell commands"

    async def execute(self, command: str) -> str:
        # implementation
        pass
```

**Go:**
```go
// Tool interface - any type with these methods satisfies it
type Tool interface {
    Name() string
    Description() string
    Execute(ctx context.Context, params map[string]any) (string, error)
}

// ShellTool implicitly implements Tool
type ShellTool struct {
    workspace string
    timeout   time.Duration
}

func (s *ShellTool) Name() string {
    return "exec"
}

func (s *ShellTool) Description() string {
    return "Execute shell commands"
}

func (s *ShellTool) Execute(ctx context.Context, params map[string]any) (string, error) {
    // implementation
}
```

**Key Differences:**
- Go interfaces are implicit (no `implements` keyword)
- Interface satisfaction is checked at compile time
- Interfaces should be small and focused

### 2.2 Callable/Duck Typing → Function Types

**Python:**
```python
def process(callback):
    # callback can be any callable
    result = callback(data)
```

**Go:**
```go
// Define explicit function type
type MessageHandler func(ctx context.Context, msg InboundMessage) error

func process(handler MessageHandler) {
    err := handler(ctx, msg)
}
```

### 2.3 Optional Parameters → Functional Options

**Python:**
```python
class Agent:
    def __init__(
        self,
        model: str = "gpt-4",
        max_iterations: int = 20,
        workspace: str | None = None,
    ):
        self.model = model
        self.max_iterations = max_iterations
        self.workspace = workspace or os.getcwd()
```

**Go:**
```go
type AgentConfig struct {
    Model         string
    MaxIterations int
    Workspace     string
}

// Option pattern for flexible configuration
type AgentOption func(*AgentConfig)

func WithModel(model string) AgentOption {
    return func(c *AgentConfig) {
        c.Model = model
    }
}

func WithMaxIterations(n int) AgentOption {
    return func(c *AgentConfig) {
        c.MaxIterations = n
    }
}

func NewAgent(opts ...AgentOption) *Agent {
    cfg := AgentConfig{
        Model:         "gpt-4",         // default
        MaxIterations: 20,              // default
        Workspace:     os.Getwd(),      // default
    }

    for _, opt := range opts {
        opt(&cfg)
    }

    return &Agent{config: cfg}
}

// Usage
agent := NewAgent(
    WithModel("claude-3"),
    WithMaxIterations(10),
)
```

---

## 3. Data Structures: Dataclasses → Structs

### 3.1 Dataclasses → Structs with Tags

**Python:**
```python
from dataclasses import dataclass, field
from datetime import datetime

@dataclass
class InboundMessage:
    channel: str
    sender_id: str
    chat_id: str
    content: str
    timestamp: datetime = field(default_factory=datetime.now)
    media: list[str] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)
```

**Go:**
```go
type InboundMessage struct {
    Channel   string         `json:"channel"`
    SenderID  string         `json:"sender_id"`
    ChatID    string         `json:"chat_id"`
    Content   string         `json:"content"`
    Timestamp time.Time      `json:"timestamp"`
    Media     []string       `json:"media,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
}

// Constructor with defaults
func NewInboundMessage(channel, senderID, chatID, content string) InboundMessage {
    return InboundMessage{
        Channel:   channel,
        SenderID:  senderID,
        ChatID:    chatID,
        Content:   content,
        Timestamp: time.Now(),
        Media:     nil,
        Metadata:  nil,
    }
}
```

**Key Differences:**
- Use struct tags for JSON serialization
- `omitempty` skips zero values in JSON
- No default field values; use constructors

### 3.2 Frozen Dataclasses → Immutable by Convention

**Python:**
```python
@dataclass(frozen=True)
class ProviderSpec:
    name: str
    keywords: tuple[str, ...]
    env_key: str
```

**Go:**
```go
// Immutability by convention - unexported fields + getters
type ProviderSpec struct {
    name     string
    keywords []string
    envKey   string
}

func (p ProviderSpec) Name() string     { return p.name }
func (p ProviderSpec) Keywords() []string { return p.keywords }
func (p ProviderSpec) EnvKey() string   { return p.envKey }

// Or simply use exported fields and document immutability
type ProviderSpec struct {
    Name     string   // Do not modify after creation
    Keywords []string // Do not modify after creation
    EnvKey   string   // Do not modify after creation
}
```

---

## 4. Error Handling: Exceptions → Error Returns

### 4.1 Try/Except → if err != nil

**Python:**
```python
async def execute_tool(self, name: str, params: dict) -> str:
    try:
        tool = self.tools[name]
        result = await tool.execute(**params)
        return result
    except KeyError:
        return f"Tool not found: {name}"
    except ValidationError as e:
        return f"Invalid params: {e}"
    except Exception as e:
        logger.exception("Tool execution failed")
        return f"Error: {e}"
```

**Go:**
```go
func (r *Registry) ExecuteTool(ctx context.Context, name string, params map[string]any) (string, error) {
    tool, ok := r.tools[name]
    if !ok {
        return "", fmt.Errorf("tool not found: %s", name)
    }

    if err := validateParams(tool.Parameters(), params); err != nil {
        return "", fmt.Errorf("invalid params: %w", err)
    }

    result, err := tool.Execute(ctx, params)
    if err != nil {
        slog.Error("tool execution failed", "tool", name, "error", err)
        return "", fmt.Errorf("execute %s: %w", name, err)
    }

    return result, nil
}
```

### 4.2 Custom Exceptions → Sentinel Errors

**Python:**
```python
class ToolNotFoundError(Exception):
    pass

class MaxIterationsError(Exception):
    pass

# Usage
try:
    result = await process()
except ToolNotFoundError:
    # handle
except MaxIterationsError:
    # handle
```

**Go:**
```go
// Define sentinel errors
var (
    ErrToolNotFound   = errors.New("tool not found")
    ErrMaxIterations  = errors.New("max iterations reached")
    ErrInvalidParams  = errors.New("invalid parameters")
)

// Usage
result, err := process(ctx)
if errors.Is(err, ErrToolNotFound) {
    // handle
} else if errors.Is(err, ErrMaxIterations) {
    // handle
}
```

### 4.3 Error Wrapping

**Python:**
```python
try:
    result = await self.llm.chat(messages)
except Exception as e:
    raise RuntimeError(f"LLM chat failed: {e}") from e
```

**Go:**
```go
result, err := a.llm.Chat(ctx, messages)
if err != nil {
    return nil, fmt.Errorf("llm chat: %w", err)
}
```

**Unwrapping errors:**
```go
// Check wrapped error
if errors.Is(err, context.DeadlineExceeded) {
    // Handle timeout
}

// Extract specific error type
var apiErr *APIError
if errors.As(err, &apiErr) {
    fmt.Println("API error code:", apiErr.Code)
}
```

---

## 5. Context Managers → defer

### 5.1 File Handling

**Python:**
```python
async with aiofiles.open(path, 'r') as f:
    content = await f.read()
```

**Go:**
```go
f, err := os.Open(path)
if err != nil {
    return "", fmt.Errorf("open file: %w", err)
}
defer f.Close()

content, err := io.ReadAll(f)
if err != nil {
    return "", fmt.Errorf("read file: %w", err)
}
```

### 5.2 HTTP Clients

**Python:**
```python
async with aiohttp.ClientSession() as session:
    async with session.get(url) as response:
        data = await response.text()
```

**Go:**
```go
resp, err := http.Get(url)
if err != nil {
    return "", fmt.Errorf("http get: %w", err)
}
defer resp.Body.Close()

data, err := io.ReadAll(resp.Body)
if err != nil {
    return "", fmt.Errorf("read body: %w", err)
}
```

### 5.3 Locks

**Python:**
```python
async with self._lock:
    # critical section
```

**Go:**
```go
r.mu.Lock()
defer r.mu.Unlock()
// critical section
```

---

## 6. Collections

### 6.1 Lists → Slices

**Python:**
```python
messages: list[Message] = []
messages.append(msg)
messages.extend(more_messages)
first = messages[0]
last = messages[-1]
subset = messages[1:3]
```

**Go:**
```go
var messages []Message
messages = append(messages, msg)
messages = append(messages, moreMessages...)
first := messages[0]
last := messages[len(messages)-1]
subset := messages[1:3]
```

### 6.2 Dicts → Maps

**Python:**
```python
tools: dict[str, Tool] = {}
tools[name] = tool
tool = tools.get(name)
if name in tools:
    del tools[name]
for name, tool in tools.items():
    print(name, tool)
```

**Go:**
```go
tools := make(map[string]Tool)
tools[name] = tool
tool, ok := tools[name]  // ok is false if not found
if _, exists := tools[name]; exists {
    delete(tools, name)
}
for name, tool := range tools {
    fmt.Println(name, tool)
}
```

### 6.3 Sets → map[T]struct{}

**Python:**
```python
seen: set[str] = set()
seen.add(item)
if item in seen:
    pass
seen.remove(item)
```

**Go:**
```go
seen := make(map[string]struct{})
seen[item] = struct{}{}
if _, exists := seen[item]; exists {
    // exists
}
delete(seen, item)
```

---

## 7. String Formatting

**Python:**
```python
name = "exec"
error = "not found"
msg = f"Tool {name} error: {error}"
```

**Go:**
```go
name := "exec"
err := "not found"
msg := fmt.Sprintf("Tool %s error: %s", name, err)
```

---

## 8. JSON Handling

### 8.1 Serialization

**Python:**
```python
import json

data = {"name": "test", "value": 123}
json_str = json.dumps(data)
parsed = json.loads(json_str)
```

**Go:**
```go
import "encoding/json"

data := map[string]any{"name": "test", "value": 123}

jsonBytes, err := json.Marshal(data)
if err != nil {
    return err
}
jsonStr := string(jsonBytes)

var parsed map[string]any
if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
    return err
}
```

### 8.2 Struct Serialization

**Go:**
```go
type Message struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}

msg := Message{Role: "user", Content: "hello"}

// Marshal
data, _ := json.Marshal(msg)
// {"role":"user","content":"hello"}

// Unmarshal
var parsed Message
json.Unmarshal(data, &parsed)
```

---

## 9. Logging

**Python:**
```python
from loguru import logger

logger.info("Agent started", model=model)
logger.error("Tool failed", tool=name, error=str(e))
logger.debug("Message received", content=msg.content)
```

**Go:**
```go
import "log/slog"

slog.Info("agent started", "model", model)
slog.Error("tool failed", "tool", name, "error", err)
slog.Debug("message received", "content", msg.Content)
```

---

## 10. Testing

### 10.1 pytest → testing + testify

**Python:**
```python
import pytest

@pytest.fixture
def agent():
    return Agent(model="test")

def test_process_message(agent):
    result = agent.process("hello")
    assert result == "world"

@pytest.mark.asyncio
async def test_async_process(agent):
    result = await agent.async_process("hello")
    assert "world" in result
```

**Go:**
```go
import (
    "testing"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestProcessMessage(t *testing.T) {
    agent := NewAgent(WithModel("test"))

    result, err := agent.Process(context.Background(), "hello")

    require.NoError(t, err)
    assert.Equal(t, "world", result)
}

func TestAsyncProcess(t *testing.T) {
    agent := NewAgent(WithModel("test"))

    result, err := agent.AsyncProcess(context.Background(), "hello")

    require.NoError(t, err)
    assert.Contains(t, result, "world")
}
```

### 10.2 Table-Driven Tests

**Go:**
```go
func TestShellTool(t *testing.T) {
    tests := []struct {
        name    string
        command string
        want    string
        wantErr bool
    }{
        {"echo", "echo hello", "hello", false},
        {"invalid", "nonexistent-cmd", "", true},
        {"pwd", "pwd", "/", false},
    }

    tool := NewShellTool("/", 10*time.Second, false)

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := tool.Execute(context.Background(), map[string]any{
                "command": tt.command,
            })

            if tt.wantErr {
                assert.Error(t, err)
            } else {
                assert.NoError(t, err)
                assert.Contains(t, result, tt.want)
            }
        })
    }
}
```

### 10.3 Mocking with Interfaces

**Go:**
```go
// Define interface for what you need to mock
type LLMClient interface {
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Mock implementation
type MockLLMClient struct {
    Response *ChatResponse
    Err      error
}

func (m *MockLLMClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    return m.Response, m.Err
}

// Test with mock
func TestAgentWithMock(t *testing.T) {
    mock := &MockLLMClient{
        Response: &ChatResponse{Content: "test response"},
    }

    agent := NewAgent(WithLLMClient(mock))
    result, err := agent.Process(context.Background(), "hello")

    require.NoError(t, err)
    assert.Equal(t, "test response", result)
}
```

---

## 11. Module System: Packages → Go Modules

**Python:**
```
nanobot/
├── __init__.py
├── agent/
│   ├── __init__.py
│   ├── loop.py
│   └── tools/
│       ├── __init__.py
│       ├── base.py
│       └── shell.py
```

**Go:**
```
nanosec/
├── go.mod
├── go.sum
├── internal/
│   ├── agent/
│   │   ├── agent.go
│   │   └── loop.go
│   └── tool/
│       ├── tool.go
│       ├── registry.go
│       └── shell.go
```

**Key Differences:**
- `go.mod` declares module path and dependencies
- `internal/` prevents external imports
- Package = directory (one package per directory)
- No `__init__.py` equivalent

---

## 12. Summary: Key Mental Model Shifts

| Python Concept | Go Equivalent | Notes |
|----------------|---------------|-------|
| `async/await` | Goroutines + channels | Explicit concurrency |
| `asyncio.Queue` | `chan T` | Typed channels |
| `ABC` | `interface` | Implicit satisfaction |
| `dataclass` | `struct` | With JSON tags |
| `try/except` | `if err != nil` | Explicit error handling |
| `with` | `defer` | Cleanup on function exit |
| `dict` | `map[K]V` | Must initialize with `make()` |
| `list` | `[]T` (slice) | Dynamic arrays |
| `set` | `map[T]struct{}` | No built-in set type |
| `None` | `nil` | Zero value for pointers, maps, slices, channels |
| `*args, **kwargs` | Variadic `...T` or options pattern | Less flexible |
| `@decorator` | Middleware functions | Manual composition |
| `import module` | `import "path/to/pkg"` | Full path imports |

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
