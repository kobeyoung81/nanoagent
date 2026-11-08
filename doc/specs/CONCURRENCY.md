# Concurrency Model Specification

**Version**: 1.0
**Date**: 2026-02-08
**Status**: Draft

This document specifies the concurrency architecture for nanosec.

---

## 1. Goroutine Architecture

### 1.1 Overview

Nanobot-go uses a structured concurrency model with clearly defined goroutine lifecycles:

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           Main Goroutine                                 │
│  - CLI parsing                                                          │
│  - Signal handling (SIGINT, SIGTERM)                                    │
│  - Root context management                                              │
│  - Graceful shutdown orchestration                                      │
└────────────────────────────────┬────────────────────────────────────────┘
                                 │
         ┌───────────────────────┼───────────────────────┐
         │                       │                       │
         ▼                       ▼                       ▼
┌─────────────────┐   ┌─────────────────┐   ┌─────────────────┐
│ Agent Goroutine │   │  Bus Dispatcher │   │ Channel Manager │
│                 │   │                 │   │                 │
│ - Message loop  │   │ - Outbound      │   │ - Start all     │
│ - LLM calls     │   │   routing       │   │   channels      │
│ - Tool exec     │   │ - Fan-out to    │   │                 │
│                 │   │   subscribers   │   │                 │
└─────────────────┘   └─────────────────┘   └────────┬────────┘
                                                     │
                      ┌──────────────────────────────┼──────────────────────────────┐
                      │                              │                              │
                      ▼                              ▼                              ▼
           ┌─────────────────┐           ┌─────────────────┐           ┌─────────────────┐
           │    Telegram     │           │     Discord     │           │     Feishu      │
           │    Goroutine    │           │    Goroutine    │           │    Goroutine    │
           │                 │           │                 │           │                 │
           │ - Long polling  │           │ - WebSocket     │           │ - WebSocket     │
           │ - Send messages │           │ - Gateway       │           │ - Event loop    │
           └─────────────────┘           └─────────────────┘           └─────────────────┘
```

### 1.2 Goroutine Responsibilities

| Goroutine | Responsibility | Lifetime |
|-----------|---------------|----------|
| **Main** | Signal handling, shutdown coordination | Application lifetime |
| **Agent** | Message processing, LLM calls, tool execution | Until context cancelled |
| **Bus Dispatcher** | Route outbound messages to channel subscribers | Until context cancelled |
| **Channel (each)** | Platform-specific message handling | Until context cancelled |
| **Subagent** | Background task processing | Until task completes |
| **Cron** | Scheduled task execution | Until context cancelled |

### 1.3 Goroutine Lifecycle

```go
// Standard goroutine lifecycle pattern
func (s *Service) Run(ctx context.Context) error {
    // 1. Initialization
    if err := s.init(); err != nil {
        return fmt.Errorf("init: %w", err)
    }

    // 2. Main loop
    for {
        select {
        case <-ctx.Done():
            // 3. Cleanup
            return s.cleanup()
        case item := <-s.input:
            if err := s.process(item); err != nil {
                s.handleError(err)
            }
        }
    }
}
```

---

## 2. Channel Sizing

### 2.1 Buffer Size Guidelines

| Channel | Buffer Size | Rationale |
|---------|-------------|-----------|
| `bus.inbound` | 100 | Absorb message bursts from multiple channels |
| `bus.outbound` | 100 | Buffer responses during slow channel sends |
| Subagent results | 10 | Limited concurrent subagents |
| Cron triggers | 10 | Scheduled tasks are infrequent |

### 2.2 Buffer Size Considerations

```go
// Too small: Causes blocking, reduces throughput
inbound := make(chan InboundMessage, 1)  // BAD

// Too large: Wastes memory, hides backpressure issues
inbound := make(chan InboundMessage, 10000)  // BAD

// Just right: Absorbs bursts, provides backpressure
inbound := make(chan InboundMessage, 100)  // GOOD
```

### 2.3 Backpressure Handling

```go
// Non-blocking send with backpressure detection
func (b *Bus) PublishInbound(ctx context.Context, msg InboundMessage) error {
    select {
    case <-ctx.Done():
        return ctx.Err()
    case b.inbound <- msg:
        return nil
    default:
        // Channel full - apply backpressure
        slog.Warn("inbound channel full, applying backpressure")
        select {
        case <-ctx.Done():
            return ctx.Err()
        case b.inbound <- msg:
            return nil
        case <-time.After(5 * time.Second):
            return errors.New("message dropped: channel full")
        }
    }
}
```

---

## 3. Synchronization Primitives

### 3.1 Mutex Usage

```go
// sync.RWMutex for read-heavy registries
type Registry struct {
    tools map[string]Tool
    mu    sync.RWMutex
}

func (r *Registry) Get(name string) (Tool, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    t, ok := r.tools[name]
    return t, ok
}

func (r *Registry) Register(t Tool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.tools[t.Name()] = t
}
```

### 3.2 Mutex Guidelines

| Pattern | Use Case |
|---------|----------|
| `sync.Mutex` | Exclusive access, write-heavy |
| `sync.RWMutex` | Read-heavy, occasional writes |
| `sync.Once` | One-time initialization |
| `sync.Map` | High-contention concurrent access |

### 3.3 Avoiding Deadlocks

```go
// BAD: Holding lock while calling external function
func (r *Registry) Execute(ctx context.Context, name string) (string, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()  // Lock held during Execute!
    return r.tools[name].Execute(ctx, params)  // BAD
}

// GOOD: Release lock before external call
func (r *Registry) Execute(ctx context.Context, name string) (string, error) {
    r.mu.RLock()
    t, ok := r.tools[name]
    r.mu.RUnlock()  // Release before Execute

    if !ok {
        return "", ErrToolNotFound
    }
    return t.Execute(ctx, params)  // GOOD
}
```

---

## 4. Context Usage

### 4.1 Context Hierarchy

```
                    rootCtx (with cancel)
                           │
          ┌────────────────┼────────────────┐
          │                │                │
     agentCtx         busCtx          channelMgrCtx
          │                                 │
          │                    ┌────────────┼────────────┐
          │                    │            │            │
    subagentCtx          telegramCtx   discordCtx   feishuCtx
```

### 4.2 Context Patterns

```go
// Root context with signal handling
func main() {
    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    if err := run(ctx); err != nil {
        slog.Error("application error", "error", err)
        os.Exit(1)
    }
}

// Timeout for operations
func (a *Agent) callLLM(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
    defer cancel()

    return a.provider.Chat(ctx, req)
}

// Deadline for tool execution
func (s *ShellTool) Execute(ctx context.Context, params map[string]any) (string, error) {
    ctx, cancel := context.WithTimeout(ctx, s.timeout)
    defer cancel()

    cmd := exec.CommandContext(ctx, "bash", "-c", params["command"].(string))
    return cmd.CombinedOutput()
}
```

### 4.3 Context Values

```go
// Use context values sparingly - prefer explicit parameters
type contextKey string

const (
    sessionKey contextKey = "session"
    channelKey contextKey = "channel"
)

// Setting context value
ctx = context.WithValue(ctx, sessionKey, session)

// Getting context value
if sess, ok := ctx.Value(sessionKey).(*Session); ok {
    // use sess
}
```

---

## 5. Graceful Shutdown Sequence

### 5.1 Shutdown Flow

```
Signal Received (SIGINT/SIGTERM)
           │
           ▼
┌─────────────────────────────────────────┐
│ 1. Cancel root context                  │
│    - All goroutines receive cancellation│
└────────────────────┬────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────┐
│ 2. Stop accepting new messages          │
│    - Channels stop listening            │
│    - New messages rejected              │
└────────────────────┬────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────┐
│ 3. Drain pending messages (5s timeout)  │
│    - Process messages in bus            │
│    - Complete in-flight LLM calls       │
└────────────────────┬────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────┐
│ 4. Stop channels                        │
│    - Disconnect from platforms          │
│    - Close WebSocket connections        │
└────────────────────┬────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────┐
│ 5. Close bus channels                   │
│    - Close inbound/outbound channels    │
│    - Unblock waiting goroutines         │
└────────────────────┬────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────┐
│ 6. Save state                           │
│    - Persist sessions                   │
│    - Persist cron jobs                  │
└────────────────────┬────────────────────┘
                     │
                     ▼
                   Exit
```

### 5.2 Implementation

```go
func run(ctx context.Context) error {
    // Initialize components
    bus := bus.New(100)
    agent := agent.New(cfg, bus, provider, tools, sessions)
    channelMgr := channel.NewManager(bus)

    // Start components in errgroup
    g, ctx := errgroup.WithContext(ctx)

    g.Go(func() error {
        return bus.DispatchLoop(ctx)
    })

    g.Go(func() error {
        return agent.Run(ctx)
    })

    g.Go(func() error {
        return channelMgr.StartAll(ctx)
    })

    // Wait for shutdown signal or error
    if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
        return err
    }

    // Graceful shutdown
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    return shutdown(shutdownCtx, bus, channelMgr, sessions)
}

func shutdown(ctx context.Context, bus *bus.Bus, mgr *channel.Manager, sess *session.Manager) error {
    var errs []error

    // Stop channels
    if err := mgr.StopAll(); err != nil {
        errs = append(errs, fmt.Errorf("stop channels: %w", err))
    }

    // Close bus
    if err := bus.Close(); err != nil {
        errs = append(errs, fmt.Errorf("close bus: %w", err))
    }

    // Save sessions
    if err := sess.Save(); err != nil {
        errs = append(errs, fmt.Errorf("save sessions: %w", err))
    }

    return errors.Join(errs...)
}
```

### 5.3 WaitGroup for Subagents

```go
type SubagentManager struct {
    wg sync.WaitGroup
}

func (m *SubagentManager) Spawn(ctx context.Context, task string) {
    m.wg.Add(1)
    go func() {
        defer m.wg.Done()
        // Run subagent
    }()
}

func (m *SubagentManager) Wait() {
    m.wg.Wait()
}
```

---

## 6. Error Propagation

### 6.1 errgroup for Concurrent Operations

```go
import "golang.org/x/sync/errgroup"

func (m *Manager) StartAll(ctx context.Context) error {
    g, ctx := errgroup.WithContext(ctx)

    for _, ch := range m.channels {
        ch := ch  // Capture loop variable
        g.Go(func() error {
            return ch.Start(ctx)
        })
    }

    // First error cancels all goroutines
    return g.Wait()
}
```

### 6.2 Error Handling in Goroutines

```go
// Goroutine with error channel
func processAsync(ctx context.Context, data []Item) <-chan error {
    errCh := make(chan error, 1)

    go func() {
        defer close(errCh)
        for _, item := range data {
            if err := process(ctx, item); err != nil {
                errCh <- err
                return
            }
        }
    }()

    return errCh
}

// Usage
errCh := processAsync(ctx, data)
select {
case <-ctx.Done():
    return ctx.Err()
case err := <-errCh:
    if err != nil {
        return err
    }
}
```

### 6.3 Panic Recovery

```go
// Recover panics in goroutines
func safeGo(fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("panic recovered",
                    "panic", r,
                    "stack", string(debug.Stack()),
                )
            }
        }()
        fn()
    }()
}

// Usage
safeGo(func() {
    // Code that might panic
})
```

---

## 7. Concurrency Patterns

### 7.1 Worker Pool

```go
// Worker pool for tool execution
type WorkerPool struct {
    tasks   chan Task
    results chan Result
    workers int
}

func NewWorkerPool(workers, queueSize int) *WorkerPool {
    return &WorkerPool{
        tasks:   make(chan Task, queueSize),
        results: make(chan Result, queueSize),
        workers: workers,
    }
}

func (p *WorkerPool) Start(ctx context.Context) {
    for i := 0; i < p.workers; i++ {
        go p.worker(ctx)
    }
}

func (p *WorkerPool) worker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case task := <-p.tasks:
            result := task.Execute(ctx)
            p.results <- result
        }
    }
}
```

### 7.2 Fan-out / Fan-in

```go
// Fan-out: One source to multiple workers
func fanOut(ctx context.Context, input <-chan Item, workers int) []<-chan Result {
    outputs := make([]<-chan Result, workers)

    for i := 0; i < workers; i++ {
        output := make(chan Result)
        outputs[i] = output

        go func(out chan<- Result) {
            defer close(out)
            for item := range input {
                select {
                case <-ctx.Done():
                    return
                case out <- process(item):
                }
            }
        }(output)
    }

    return outputs
}

// Fan-in: Multiple sources to one sink
func fanIn(ctx context.Context, inputs ...<-chan Result) <-chan Result {
    output := make(chan Result)

    var wg sync.WaitGroup
    for _, input := range inputs {
        wg.Add(1)
        go func(in <-chan Result) {
            defer wg.Done()
            for result := range in {
                select {
                case <-ctx.Done():
                    return
                case output <- result:
                }
            }
        }(input)
    }

    go func() {
        wg.Wait()
        close(output)
    }()

    return output
}
```

### 7.3 Rate Limiting

```go
import "golang.org/x/time/rate"

type RateLimitedClient struct {
    limiter *rate.Limiter
    client  *http.Client
}

func NewRateLimitedClient(rps float64, burst int) *RateLimitedClient {
    return &RateLimitedClient{
        limiter: rate.NewLimiter(rate.Limit(rps), burst),
        client:  &http.Client{},
    }
}

func (c *RateLimitedClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
    if err := c.limiter.Wait(ctx); err != nil {
        return nil, err
    }
    return c.client.Do(req)
}
```

---

## 8. Testing Concurrency

### 8.1 Race Detection

```bash
# Always run tests with race detector
go test -race ./...
```

### 8.2 Testing Concurrent Code

```go
func TestConcurrentAccess(t *testing.T) {
    registry := NewRegistry()

    // Register tools concurrently
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            registry.Register(&mockTool{name: fmt.Sprintf("tool_%d", n)})
        }(i)
    }
    wg.Wait()

    // Verify all tools registered
    assert.Equal(t, 100, len(registry.Names()))
}
```

### 8.3 Testing Timeouts

```go
func TestTimeout(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
    defer cancel()

    tool := NewShellTool(".", 50*time.Millisecond, false)
    _, err := tool.Execute(ctx, map[string]any{
        "command": "sleep 10",  // Will timeout
    })

    assert.Error(t, err)
    assert.True(t, errors.Is(err, context.DeadlineExceeded))
}
```

---

## 9. Performance Considerations

### 9.1 Avoiding Goroutine Leaks

```go
// BAD: Goroutine leak if context cancelled before send
go func() {
    result := compute()
    resultCh <- result  // Blocked forever if no receiver
}()

// GOOD: Select with context
go func() {
    result := compute()
    select {
    case <-ctx.Done():
        return
    case resultCh <- result:
    }
}()
```

### 9.2 Minimizing Lock Contention

```go
// BAD: Large critical section
func (s *Store) ProcessAll() {
    s.mu.Lock()
    defer s.mu.Unlock()
    for _, item := range s.items {
        process(item)  // Long operation under lock
    }
}

// GOOD: Small critical section
func (s *Store) ProcessAll() {
    s.mu.RLock()
    items := make([]Item, len(s.items))
    copy(items, s.items)
    s.mu.RUnlock()

    for _, item := range items {
        process(item)  // No lock held
    }
}
```

### 9.3 Channel vs Mutex

| Use Channels | Use Mutex |
|--------------|-----------|
| Passing ownership of data | Protecting shared state |
| Coordinating goroutines | Simple read/write access |
| Implementing pipelines | Performance-critical paths |

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
