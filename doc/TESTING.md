# Testing Strategy

**Version**: 2.0
**Date**: 2026-02-09
**Status**: Draft

This document defines the testing strategy for nanosec.

---

## 1. Test Categories

### 1.1 Test Pyramid

```
                    ┌───────────┐
                    │   E2E     │  Few, slow, high confidence
                    │   Tests   │
                    ├───────────┤
                    │Integration│  Some, medium speed
                    │   Tests   │
                    ├───────────┤
                    │   Unit    │  Many, fast, focused
                    │   Tests   │
                    └───────────┘
```

### 1.2 Test Types

| Type | Purpose | Location | Coverage Target |
|------|---------|----------|-----------------|
| Unit | Test individual functions/methods | `*_test.go` alongside code | 80%+ |
| Integration | Test package interactions | `test/integration/` | Critical paths |
| E2E | Test full system flows | `test/e2e/` | Happy paths |

---

## 2. Unit Testing

### 2.1 File Naming Convention

```
internal/
├── bus/
│   ├── server.go
│   ├── server_test.go      # Unit tests for server.go
│   ├── handler.go
│   ├── handler_test.go     # Unit tests for handler.go
│   └── store/
│       ├── user.go
│       ├── user_test.go    # Unit tests for user.go
│       ├── queue.go
│       └── queue_test.go   # Unit tests for queue.go
├── gateway/
│   ├── gateway.go
│   ├── gateway_test.go
│   └── channel/
│       ├── cli/
│       │   └── adapter.go
│       ├── wecom/
│       │   ├── adapter.go
│       │   └── adapter_test.go
│       └── feishu/
│           ├── adapter.go
│           └── adapter_test.go
├── agent/
│   ├── agent.go
│   ├── agent_test.go
│   ├── loop.go
│   └── loop_test.go
```

### 2.2 Test Function Naming

```go
// Function: TestFunctionName
func TestResolveUser(t *testing.T) { ... }

// Method: TestTypeName_MethodName
func TestServer_HandleConnection(t *testing.T) { ... }

// Scenario: TestTypeName_MethodName_Scenario
func TestServer_HandleConnection_UnknownUser(t *testing.T) { ... }
```

### 2.3 Table-Driven Tests

```go
func TestShellTool_Execute(t *testing.T) {
    tests := []struct {
        name    string
        command string
        want    string
        wantErr bool
    }{
        {
            name:    "echo command",
            command: "echo hello",
            want:    "hello",
            wantErr: false,
        },
        {
            name:    "invalid command",
            command: "nonexistent-command-xyz",
            want:    "",
            wantErr: true,
        },
        {
            name:    "empty command",
            command: "",
            want:    "",
            wantErr: true,
        },
    }

    tool := NewShellTool(".", 10*time.Second, false)

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

### 2.4 Using testify

```go
import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestExample(t *testing.T) {
    // assert: continues on failure
    assert.Equal(t, expected, actual)
    assert.Contains(t, str, substring)
    assert.True(t, condition)
    assert.Error(t, err)
    assert.NoError(t, err)

    // require: stops on failure
    require.NoError(t, err)  // Stop if error
    require.NotNil(t, obj)   // Stop if nil
}
```

---

## 3. Mocking Strategy

### 3.1 Interface-Based Mocking

```go
// Define interface for what you need to mock
type LLMClient interface {
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Production implementation
type OpenAIClient struct { ... }

func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    // Real API call
}

// Mock implementation
type MockLLMClient struct {
    Response *ChatResponse
    Err      error
    Calls    []ChatRequest  // Track calls for assertions
}

func (m *MockLLMClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    m.Calls = append(m.Calls, req)
    return m.Response, m.Err
}

// Mock Bus Client for agent/gateway tests
type MockBusClient struct {
    PostInboundFn   func(msg protocol.InboundMessage) error
    PollInboundFn   func(userID, agentID string) (*protocol.InboundMessage, error)
    PostOutboundFn  func(msg protocol.OutboundMessage) error
    PollOutboundFn  func(channel, gatewayID string) (*protocol.OutboundMessage, error)
    ResolveUserFn   func(channel, senderID string) (string, error)
}

func (m *MockBusClient) PostInbound(msg protocol.InboundMessage) error {
    if m.PostInboundFn != nil {
        return m.PostInboundFn(msg)
    }
    return nil
}

func (m *MockBusClient) ResolveUser(channel, senderID string) (string, error) {
    if m.ResolveUserFn != nil {
        return m.ResolveUserFn(channel, senderID)
    }
    return "test_user", nil
}
```

### 3.2 Using Mocks in Tests

```go
func TestAgent_ProcessMessage(t *testing.T) {
    // Create mock
    mockLLM := &MockLLMClient{
        Response: &ChatResponse{
            Content: "Hello! How can I help?",
        },
    }

    // Inject mock
    agent := NewAgent(AgentConfig{}, mockLLM, NewToolRegistry())

    // Test
    result, err := agent.ProcessDirect(context.Background(), "Hi")

    // Assert
    require.NoError(t, err)
    assert.Equal(t, "Hello! How can I help?", result)
    assert.Len(t, mockLLM.Calls, 1)
    assert.Equal(t, "Hi", mockLLM.Calls[0].Messages[0].Content)
}
```

### 3.3 Mock Tool

```go
type MockTool struct {
    name        string
    description string
    params      ParameterSchema
    result      string
    err         error
}

func (m *MockTool) Name() string                { return m.name }
func (m *MockTool) Description() string         { return m.description }
func (m *MockTool) Parameters() ParameterSchema { return m.params }
func (m *MockTool) Execute(ctx context.Context, params map[string]any) (string, error) {
    return m.result, m.err
}
```

---

## 4. Integration Testing

### 4.1 Test Structure

```
test/
├── integration/
│   ├── bus_test.go           # Bus + MySQL
│   ├── agent_test.go         # Agent + Bus + Tools + Session
│   ├── gateway_test.go       # Gateway + Bus + Mock Channel
│   ├── provider_test.go      # Provider + Real API
│   └── multiuser_test.go     # Multi-user message routing
└── testdata/
    ├── config.json           # Test configuration
    └── fixtures/             # Test fixtures
```

### 4.2 Bus Integration Test

```go
// +build integration

package integration

import (
    "context"
    "testing"

    "nanosec/internal/bus"
    "nanosec/internal/bus/store"
    "nanosec/internal/protocol"
)

func TestBusWithMySQL(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }

    // Setup test database
    db := setupTestDB(t)
    defer db.Close()

    s := store.New(db)
    server := bus.NewServer(s)

    // Start server in background
    go server.ListenAndServe(":0")
    defer server.Close()

    // Test user resolution
    err := s.RegisterUser("alice")
    require.NoError(t, err)

    err = s.LinkChannel("alice", "telegram", "123456")
    require.NoError(t, err)

    userID, err := s.ResolveUser("telegram", "123456")
    require.NoError(t, err)
    assert.Equal(t, "alice", userID)
}

func TestBusMessageFlow(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()

    s := store.New(db)

    // Post inbound message
    err := s.PostInbound(protocol.InboundMessage{
        UserID:    "alice",
        SessionID: "telegram:123",
        Channel:   "telegram",
        ChatID:    "123",
        Content:   "Hello",
    })
    require.NoError(t, err)

    // Claim message
    msg, err := s.ClaimInbound("alice", "agent-1")
    require.NoError(t, err)
    assert.Equal(t, "Hello", msg.Content)

    // Second claim should return nil (already claimed)
    msg2, err := s.ClaimInbound("alice", "agent-2")
    require.NoError(t, err)
    assert.Nil(t, msg2)
}
```

### 4.3 Multi-Process Integration Test

```go
// +build integration

package integration

import (
    "context"
    "testing"
    "time"

    "nanosec/internal/agent"
    "nanosec/internal/bus"
    "nanosec/internal/gateway"
    "nanosec/internal/protocol"
)

func TestFullMessageFlow(t *testing.T) {
    // 1. Start Bus
    db := setupTestDB(t)
    busServer := bus.NewServer(store.New(db))
    go busServer.ListenAndServe(":9999")
    defer busServer.Close()

    // 2. Register user
    store.New(db).RegisterUser("alice")
    store.New(db).LinkChannel("alice", "test", "sender-1")

    // 3. Start Agent for alice
    mockLLM := &MockLLMClient{
        Response: &provider.ChatResponse{Content: "Hello from agent!"},
    }
    agentCfg := agent.Config{UserID: "alice", BusURL: "ws://localhost:9999"}
    a := agent.New(agentCfg, mockLLM, tool.NewRegistry())
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    go a.Run(ctx)

    // 4. Simulate gateway posting inbound
    client := protocol.NewClient("ws://localhost:9999")
    err := client.PostInbound(protocol.InboundMessage{
        UserID:    "alice",
        SessionID: "test:sender-1",
        Channel:   "test",
        ChatID:    "sender-1",
        Content:   "Hi",
    })
    require.NoError(t, err)

    // 5. Wait and poll outbound
    time.Sleep(500 * time.Millisecond)
    msg, err := client.PollOutbound("test", "gw-1")
    require.NoError(t, err)
    assert.Equal(t, "Hello from agent!", msg.Content)
}
```

### 4.4 Provider Integration Test

```go
// +build integration

package integration

import (
    "context"
    "os"
    "strings"
    "testing"

    "nanosec/internal/provider"
)

func TestOpenAICompatibleProvider(t *testing.T) {
    apiKey := os.Getenv("LLM_API_KEY")
    baseURL := os.Getenv("LLM_BASE_URL")
    model := os.Getenv("LLM_MODEL")

    if apiKey == "" {
        t.Skip("LLM_API_KEY not set")
    }
    if baseURL == "" {
        baseURL = "https://api.openai.com/v1"
    }
    if model == "" {
        model = "gpt-3.5-turbo"
    }

    p := provider.New(provider.Config{
        APIKey:       apiKey,
        BaseURL:      baseURL,
        DefaultModel: model,
    })

    resp, err := p.Chat(context.Background(), provider.ChatRequest{
        Messages: []provider.Message{
            {Role: "user", Content: "Say 'test' and nothing else"},
        },
    })

    require.NoError(t, err)
    assert.Contains(t, strings.ToLower(resp.Content), "test")
}
```

---

## 5. End-to-End Testing

### 5.1 E2E Test Structure

```
test/
└── e2e/
    ├── cli_test.go           # CLI command tests
    ├── bus_test.go           # Bus service startup
    ├── agent_test.go         # Agent service startup
    ├── gateway_test.go       # Gateway service startup
    └── multiuser_test.go     # Multi-user full flow
```

### 5.2 CLI E2E Test

```go
// +build e2e

package e2e

import (
    "os/exec"
    "testing"
)

func TestCLI_Version(t *testing.T) {
    cmd := exec.Command("go", "run", "../../cmd/nanosec", "version")
    output, err := cmd.CombinedOutput()

    require.NoError(t, err)
    assert.Contains(t, string(output), "nanosec")
}

func TestCLI_Bus(t *testing.T) {
    // Start bus in background
    cmd := exec.Command("go", "run", "../../cmd/nanobus", "--config", "testdata/bus.json")
    err := cmd.Start()
    require.NoError(t, err)
    defer cmd.Process.Kill()

    // Wait for startup
    time.Sleep(time.Second)

    // Verify WebSocket is listening
    conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:9000", nil)
    require.NoError(t, err)
    conn.Close()
}

func TestCLI_Agent(t *testing.T) {
    // Requires bus to be running
    cmd := exec.Command("go", "run", "../../cmd/nanosec", "--user=testuser", "--config", "testdata/agent.json")
    output, err := cmd.CombinedOutput()

    // Should fail gracefully if bus not available
    assert.Contains(t, string(output), "connecting to bus")
}

func TestCLI_Gateway(t *testing.T) {
    cmd := exec.Command("go", "run", "../../cmd/nanogateway", "--config", "testdata/gateway.json")
    output, err := cmd.CombinedOutput()

    assert.Contains(t, string(output), "gateway")
}
```

---

## 6. Test Data Management

### 6.1 testdata Directory

```
internal/
├── config/
│   ├── loader.go
│   ├── loader_test.go
│   └── testdata/
│       ├── valid_config.json
│       ├── invalid_config.json
│       └── minimal_config.json
```

### 6.2 Golden Files

```go
func TestContextBuilder_Build(t *testing.T) {
    builder := NewContextBuilder().WithSystemPrompt("You are a helpful assistant")

    session := session.NewSession("test")
    session.AddMessage(provider.Message{Role: "user", Content: "Hello"})

    result := builder.Build(session)

    // Compare against golden file
    golden := filepath.Join("testdata", "context_output.golden.json")
    if *update {
        // Update golden file
        data, _ := json.MarshalIndent(result, "", "  ")
        os.WriteFile(golden, data, 0644)
    }

    expected, _ := os.ReadFile(golden)
    actual, _ := json.MarshalIndent(result, "", "  ")
    assert.Equal(t, string(expected), string(actual))
}
```

### 6.3 Fixtures

```go
func loadFixture(t *testing.T, name string) []byte {
    t.Helper()
    data, err := os.ReadFile(filepath.Join("testdata", "fixtures", name))
    require.NoError(t, err)
    return data
}

func TestParseMessage(t *testing.T) {
    data := loadFixture(t, "telegram_message.json")

    var msg InboundMessage
    err := json.Unmarshal(data, &msg)

    require.NoError(t, err)
    assert.Equal(t, "telegram", msg.Channel)
}
```

---

## 7. Concurrency Testing

### 7.1 Race Detection

```bash
# Always run with race detector
go test -race ./...

# In CI
go test -race -coverprofile=coverage.out ./...
```

### 7.2 Concurrent Access Tests

```go
func TestRegistry_ConcurrentAccess(t *testing.T) {
    registry := NewRegistry()

    var wg sync.WaitGroup

    // Concurrent writes
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            registry.Register(&MockTool{name: fmt.Sprintf("tool_%d", n)})
        }(i)
    }

    // Concurrent reads
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            registry.Get(fmt.Sprintf("tool_%d", n))
        }(i)
    }

    wg.Wait()

    // Verify no data corruption
    assert.Equal(t, 100, len(registry.Names()))
}
```

### 7.3 Timeout Tests

```go
func TestShellTool_Timeout(t *testing.T) {
    tool := NewShellTool(".", 100*time.Millisecond, false)

    ctx := context.Background()
    _, err := tool.Execute(ctx, map[string]any{
        "command": "sleep 10",
    })

    assert.Error(t, err)
    assert.True(t, errors.Is(err, context.DeadlineExceeded))
}
```

---

## 8. CI Pipeline

### 8.1 GitHub Actions Workflow

```yaml
# .github/workflows/ci.yml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Install dependencies
        run: go mod download

      - name: Run linter
        uses: golangci/golangci-lint-action@v4
        with:
          version: latest

      - name: Run tests
        run: go test -race -coverprofile=coverage.out ./...

      - name: Check coverage
        run: |
          go tool cover -func=coverage.out | grep total | awk '{print $3}'
          COVERAGE=$(go tool cover -func=coverage.out | grep total | awk '{print substr($3, 1, length($3)-1)}')
          if (( $(echo "$COVERAGE < 80" | bc -l) )); then
            echo "Coverage $COVERAGE% is below 80%"
            exit 1
          fi

      - name: Upload coverage
        uses: codecov/codecov-action@v4
        with:
          files: coverage.out

  build:
    runs-on: ubuntu-latest
    needs: test

    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Build
        run: |
          go build -o nanobus ./cmd/nanobus
          go build -o nanogateway ./cmd/nanogateway
          go build -o nanosec ./cmd/nanosec

      - name: Verify binaries
        run: |
          ./nanobus version
          ./nanogateway version
          ./nanosec version

  integration:
    runs-on: ubuntu-latest
    needs: build
    if: github.event_name == 'push'

    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Run integration tests
        run: go test -tags=integration ./test/integration/...
        env:
          LLM_API_KEY: ${{ secrets.LLM_API_KEY }}
          TEST_MYSQL_DSN: ${{ secrets.TEST_MYSQL_DSN }}
```

### 8.2 golangci-lint Configuration

```yaml
# .golangci.yml
run:
  timeout: 5m

linters:
  enable:
    - errcheck
    - gosimple
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gofmt
    - goimports
    - misspell
    - unconvert
    - unparam
    - nakedret
    - prealloc
    - exportloopref

linters-settings:
  errcheck:
    check-type-assertions: true
  govet:
    check-shadowing: true
  nakedret:
    max-func-lines: 30

issues:
  exclude-use-default: false
  max-issues-per-linter: 0
  max-same-issues: 0
```

---

## 9. Test Commands

### 9.1 Makefile Targets

```makefile
.PHONY: test test-race test-cover test-integration test-e2e lint

# Run all unit tests
test:
	go test ./...

# Run tests with race detector
test-race:
	go test -race ./...

# Run tests with coverage
test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Run integration tests
test-integration:
	go test -tags=integration ./test/integration/...

# Run E2E tests
test-e2e:
	go test -tags=e2e ./test/e2e/...

# Run linter
lint:
	golangci-lint run

# Run all checks (CI)
ci: lint test-race test-cover
	@echo "All checks passed!"
```

### 9.2 Common Test Commands

```bash
# Run all tests
go test ./...

# Run tests in a specific package
go test ./internal/bus/...

# Run a specific test
go test -run TestBus_PublishInbound ./internal/bus/

# Run tests with verbose output
go test -v ./...

# Run tests with coverage
go test -cover ./...

# Run tests with race detector
go test -race ./...

# Run short tests only
go test -short ./...

# Run tests matching a pattern
go test -run "Test.*Timeout" ./...
```

---

## 10. Best Practices

### 10.1 Test Isolation

```go
// Each test should be independent
func TestSomething(t *testing.T) {
    // Setup - create fresh state
    registry := NewRegistry()

    // Test
    registry.Register(tool)

    // No cleanup needed - test ends
}
```

### 10.2 Parallel Tests

```go
func TestParallel(t *testing.T) {
    t.Parallel()  // Mark as safe for parallel execution

    // Test code...
}

func TestParallelSubtests(t *testing.T) {
    tests := []struct{ name string }{{"a"}, {"b"}, {"c"}}

    for _, tt := range tests {
        tt := tt  // Capture loop variable
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()  // Subtests run in parallel
            // Test code...
        })
    }
}
```

### 10.3 Test Helpers

```go
// Helpers should call t.Helper()
func assertNoError(t *testing.T, err error) {
    t.Helper()  // Reports caller's line number on failure
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}

// Use t.TempDir() for temporary directories
func TestFileOperations(t *testing.T) {
    dir := t.TempDir()  // Automatically cleaned up
    // Use dir...
}
```

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
| 2.0 | 2026-02-09 | Claude | Multi-user three-process architecture tests |
| 2.1 | 2026-02-22 | Claude | Fixed channel references (telegram/discord → wecom/feishu/cli) |
