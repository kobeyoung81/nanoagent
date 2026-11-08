# Test Progress

This file tracks the testing progress of Nanosec based on the [TESTING.md](../doc/TESTING.md) strategy.

**Last Updated:** 2026-09-01

---

## Coverage Overview

| Package | Target | Current | Status |
|---------|--------|---------|--------|
| `internal/agent` | 80%+ | 41.9% | 🟡 In Progress |
| `internal/bus` | 80%+ | 53.0% | 🟡 In Progress |
| `internal/bus/store` | 80%+ | 68.4% | 🟡 In Progress |
| `internal/config` | 80%+ | 92.3% | ✅ Completed |
| `internal/cron` | 80%+ | 89.7% | ✅ Completed |
| `internal/gateway` | 80%+ | 0.0% | ⬜ Not Started |
| `internal/gateway/channel/cli` | 80%+ | 0.0% | ⬜ Not Started |
| `internal/gateway/channel/feishu` | 80%+ | 56.6% | 🟡 In Progress |
| `internal/gateway/channel/wecom` | 80%+ | 62.3% | 🟡 In Progress |
| `internal/protocol` | 80%+ | 63.6% | 🟡 In Progress |
| `internal/provider` | 80%+ | 26.0% | 🟡 In Progress |
| `internal/session` | 80%+ | 89.2% | ✅ Completed |
| `internal/skill` | 80%+ | 81.5% | ✅ Completed |
| `internal/tool` | 80%+ | 69.7% | 🟡 In Progress |
| **Overall** | 80%+ | **~55%** | 🟡 In Progress |

**Status Legend:** ⬜ Not Started | 🟡 In Progress | ✅ Completed | ⏸️ Blocked

---

## Unit Tests

### bus Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestHandler_ResolveUser | Resolve user identity | ✅ | handler_test.go |
| TestHandler_ResolveUser_NotFound | Unknown sender returns error | ✅ | handler_test.go |
| TestHandler_PostInbound | Post inbound message | ✅ | handler_test.go |
| TestHandler_PostOutbound | Post outbound message | ✅ | handler_test.go |
| TestHandler_PollInbound_WithMessage | Poll inbound message | ✅ | handler_test.go |
| TestHandler_PollOutbound_WithMessage | Poll outbound message | ✅ | handler_test.go |
| TestHandler_UnknownType | Unknown message type | ✅ | handler_test.go |
| TestServer_Close | Graceful shutdown | ⬜ | Deferred - manual testing |

### bus/store Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestNew | Create store with DB | ✅ | user_test.go |
| TestRegisterUser | Register new user | ✅ | user_test.go |
| TestLinkChannel | Link channel identity to user | ✅ | user_test.go |
| TestResolveUser | Resolve (channel, sender_id) → user_id | ✅ | user_test.go |
| TestResolveUser_NotFound | Unknown sender returns error | ✅ | user_test.go |
| TestListUsers | List all users | ✅ | user_test.go |
| TestPostInbound | Insert inbound message | ✅ | queue_test.go |
| TestClaimInbound | Atomic claim of inbound message | ✅ | queue_test.go |
| TestClaimInbound_NoPending | No pending messages returns nil | ✅ | queue_test.go |
| TestPostOutbound | Insert outbound message | ✅ | queue_test.go |
| TestClaimOutbound | Atomic claim of outbound message | ✅ | queue_test.go |
| TestClaimOutbound_NoPending | No pending messages returns nil | ✅ | queue_test.go |

### gateway Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestGateway_New | Create gateway with bus client | ⬜ | |
| TestGateway_HandleInbound | Handle inbound from channel | ⬜ | |
| TestGateway_HandleInbound_UnknownUser | Unknown user ignored | ⬜ | |
| TestGateway_PollOutbound | Poll and send outbound | ⬜ | |
| TestBusClient_ResolveUser | Resolve user via WebSocket | ⬜ | |
| TestBusClient_PostInbound | Post inbound via WebSocket | ⬜ | |
| TestBusClient_Reconnect | Auto-reconnect on disconnect | ⬜ | |

### agent Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestAgent_ProcessMessage | Process message from bus | ✅ | agent_test.go |
| TestAgent_ProcessMessage_ToolUse | Process message with tool calls | ✅ | agent_test.go |
| TestAgent_MaxIterations | Max iterations reached error | ✅ | agent_test.go |
| TestContextBuilder_MemoryLoading | MEMORY.md included in prompt | ✅ | context_test.go |
| TestContextBuilder_DailyNotesLoading | Today's date file included | ✅ | context_test.go |
| TestContextBuilder_NoMemoryFiles | No memory section when absent | ✅ | context_test.go |
| TestContextBuilder_BothMemoryFiles | Both memory files included | ✅ | context_test.go |

### tool Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestRegistry | Register/Get/Execute/Definitions | ✅ | tool_test.go |
| TestShellTool | Execute shell command | ✅ | tool_test.go |
| TestFilesystemTools | Safe path, read/write/list | ✅ | filesystem_test.go |
| TestEditFileTool_BasicReplace | Successful find-and-replace | ✅ | filesystem_test.go |
| TestEditFileTool_NotFound | old_text not in file returns error | ✅ | filesystem_test.go |
| TestEditFileTool_Ambiguous | old_text appears multiple times | ✅ | filesystem_test.go |
| TestEditFileTool_WorkspaceRestriction | Path outside workspace denied | ✅ | filesystem_test.go |
| TestEditFileTool_EmptyNewText | Deletion (replace with empty) | ✅ | filesystem_test.go |
| TestAppendNoteTool_CreateNew | Creates daily file with header | ✅ | filesystem_test.go |
| TestAppendNoteTool_AppendExisting | Appends to existing file | ✅ | filesystem_test.go |
| TestAppendNoteTool_MultipleEntries | Multiple timestamped lines | ✅ | filesystem_test.go |
| TestAppendNoteTool_CreatesMemoryDir | Creates memory/ dir if missing | ✅ | filesystem_test.go |
| TestShellTool_DenyList_RmRf | rm -rf blocked | ✅ | tool_test.go |
| TestShellTool_DenyList_Mkfs | mkfs blocked | ✅ | tool_test.go |
| TestShellTool_DenyList_ForkBomb | Fork bomb blocked | ✅ | tool_test.go |
| TestShellTool_DenyList_DD | dd raw write blocked | ✅ | tool_test.go |
| TestShellTool_DenyList_SafeRm | rm file.txt NOT blocked | ✅ | tool_test.go |
| TestShellTool_DenyList_SafeCommand | ls, echo, pwd pass through | ✅ | tool_test.go |
| TestMessageTool | Send message via bus | ✅ | message_test.go |
| TestWebFetchTool | Fetch URL | ✅ | web_test.go |
| TestWebSearchTool | Search validation | ✅ | web_test.go |

### gateway/channel Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestNew_Valid (feishu) | Create Feishu adapter | ✅ | adapter_test.go |
| TestNew_MissingAppID | Missing appID error | ✅ | adapter_test.go |
| TestNew_MissingAppSecret | Missing appSecret error | ✅ | adapter_test.go |
| TestExtractSenderID | Sender ID fallback chain | ✅ | 7 subtests |
| TestExtractMessageContent | Content extraction | ✅ | 6 subtests |
| TestStringValue | Nil-safe string deref | ✅ | adapter_test.go |
| TestHandleMessageReceive | Full message flow | ✅ | adapter_test.go |
| TestHandleMessageReceive_NilEvent | Nil event ignored | ✅ | adapter_test.go |
| TestHandleMessageReceive_EmptyChatID | Empty chatID ignored | ✅ | adapter_test.go |
| TestHandleMessageReceive_EmptyContent | Empty content ignored | ✅ | adapter_test.go |
| TestNew_Valid (wecom) | Create WeCom adapter | ✅ | adapter_test.go |
| TestNew_MissingCorpID | Missing corpID error | ✅ | adapter_test.go |
| TestNew_MissingCorpSecret | Missing corpSecret error | ✅ | adapter_test.go |
| TestNew_InvalidAgentID | Zero agentID error | ✅ | adapter_test.go |
| TestNew_MissingToken | Missing token error | ✅ | adapter_test.go |
| TestNew_MissingEncodingAESKey | Missing AES key error | ✅ | adapter_test.go |
| TestNew_DefaultCallbackAddr | Default addr :8081 | ✅ | adapter_test.go |
| TestAdapter_StartStop | HTTP server lifecycle | ✅ | adapter_test.go |
| TestOnIncomingMessage_Nil | Nil message ignored | ✅ | adapter_test.go |
| TestExtractTextContent_Nil | Nil safety | ✅ | adapter_test.go |

### protocol Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestNewMessage | Create message with payload | ✅ | |
| TestNewErrorMessage | Create error message | ✅ | |
| TestMessage_DecodePayload | Decode payload from message | ✅ | |
| TestMessage_DecodePayload_EmptyPayload | Error on nil payload | ✅ | |
| TestMessage_MarshalJSON | Marshal/unmarshal envelope | ✅ | |
| TestInboundMessage_MarshalRoundTrip | Round-trip inbound message | ✅ | |
| TestOutboundMessage_MarshalRoundTrip | Round-trip outbound message | ✅ | |
| TestOutboundMessage_OmitsEmptyReplyTo | Omitempty check | ✅ | |
| TestResolveUserRequest_MarshalRoundTrip | Round-trip resolve request | ✅ | |
| TestPollInboundRequest_MarshalRoundTrip | Round-trip poll inbound | ✅ | |
| TestPollOutboundRequest_MarshalRoundTrip | Round-trip poll outbound | ✅ | |
| TestNewMessage_AsPayloadInEnvelope | Full envelope flow | ✅ | |
| TestTypeConstants | All type constants distinct | ✅ | |
| TestNewClient | Create WebSocket client | ✅ | client_test.go |
| TestNewClient_BadURL | Invalid URL returns error | ✅ | client_test.go |
| TestClient_Send | Send message over WebSocket | ✅ | client_test.go |
| TestClient_ResolveUser | Resolve user via WebSocket | ✅ | client_test.go |
| TestClient_ResolveUser_Error | ResolveUser error handling | ✅ | client_test.go |
| TestClient_PostInbound | Post inbound via WebSocket | ✅ | client_test.go |
| TestClient_PollInbound_WithMessage | Poll inbound with message | ✅ | client_test.go |
| TestClient_PostOutbound | Post outbound via WebSocket | ✅ | client_test.go |
| TestClient_PollOutbound_WithMessage | Poll outbound with message | ✅ | client_test.go |
| TestClient_Close | Close WebSocket connection | ✅ | client_test.go |

### provider Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestNewOpenAI | Create provider with config | ✅ | openai_test.go |
| TestMockProvider | Mock provider behavior | ✅ | provider_test.go |
| TestMockProvider_ResponseFunc | Dynamic mock response | ✅ | provider_test.go |

### session Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestSession_AddMessage | Add message | ✅ | session_test.go |
| TestSession_Clear | Clear session | ✅ | session_test.go |
| TestSession_GetMessages_Copy | Verify copy semantics | ✅ | session_test.go |
| TestStore_SaveLoad | Save and load from file | ✅ | store_test.go |
| TestStore_GetOrCreate | Singleton behavior | ✅ | store_test.go |
| TestStore_LoadMetadata | Metadata persistence | ✅ | store_test.go |

### config Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestDefaultBusConfig | Default bus values | ✅ | |
| TestDefaultGatewayConfig | Default gateway values | ✅ | |
| TestDefaultAgentConfig | Default agent values | ✅ | |
| TestLoadBusConfig_FromFile | Load from JSON file | ✅ | |
| TestLoadBusConfig_EmptyPath | Defaults when no file | ✅ | |
| TestLoadBusConfig_MissingFile | Error for missing file | ✅ | |
| TestLoadGatewayConfig_FromFile | Load gateway from file | ✅ | Expanded with new WeCom fields |
| TestLoadGatewayConfig_FeishuFields | Load Feishu fields from file | ✅ | config_test.go |
| TestLoadGatewayConfig_BackwardCompatibility | Old config loads fine | ✅ | config_test.go |
| TestLoadAgentConfig_FromFile | Load agent from file | ✅ | |
| TestValidateBusConfig_Valid | Valid bus config | ✅ | |
| TestValidateBusConfig_MissingDSN | Missing DSN error | ✅ | |
| TestValidateBusConfig_InvalidPort | Invalid port error | ✅ | |
| TestValidateAgentConfig_Valid | Valid agent config | ✅ | |
| TestValidateAgentConfig_MissingAPIKey | Missing API key error | ✅ | |
| TestValidateAgentConfig_InvalidIterations | Zero iterations error | ✅ | |
| TestValidateGatewayConfig_Valid | Valid gateway config | ✅ | |
| TestValidateGatewayConfig_MissingBusURL | Missing bus URL error | ✅ | |

### skill Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestLoader_List_Builtin | List built-in skills (github, weather, tmux) | ✅ | loader_test.go |
| TestLoader_List_WorkspaceOverride | Workspace skill overrides builtin | ✅ | loader_test.go |
| TestLoader_Load_Builtin | Load builtin github skill body | ✅ | loader_test.go |
| TestLoader_Load_NotFound | Unknown skill returns empty | ✅ | loader_test.go |
| TestLoader_Load_WorkspaceSkill | Load custom workspace skill | ✅ | loader_test.go |
| TestLoader_BuildSummary | XML skills summary generation | ✅ | loader_test.go |
| TestLoader_GetAlwaysSkills | Always-on skills list | ✅ | loader_test.go |
| TestParseFrontmatter_WithFrontmatter | Parse YAML frontmatter | ✅ | loader_test.go |
| TestParseFrontmatter_NoFrontmatter | Handle missing frontmatter | ✅ | loader_test.go |
| TestParseFrontmatter_WithMetadataJSON | Parse requireBins from metadata | ✅ | loader_test.go |
| TestRegistry_Get_Cached | Cache hit on second load | ✅ | loader_test.go |
| TestRegistry_BuildSummary | Registry delegates to loader | ✅ | loader_test.go |
| TestXmlEscape | XML entity escaping | ✅ | loader_test.go |

### cron Package

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestService_Start | Start scheduler | ✅ | service_test.go |
| TestService_Stop | Stop scheduler | ✅ | service_test.go |
| TestService_AddJob | Add job | ✅ | service_test.go |
| TestService_RemoveJob | Remove job | ✅ | service_test.go |
| TestService_ListJobs | List jobs | ✅ | service_test.go |
| TestService_JobFires | Job fires on schedule | ✅ | service_test.go |
| TestService_Persistence | Jobs persist across restarts | ✅ | service_test.go |
| TestComputeNext_Every | Interval scheduling | ✅ | service_test.go |
| TestComputeNext_At_InPast | Expired "at" returns nil | ✅ | service_test.go |
| TestComputeNext_CronExpr | Cron expression parsing | ✅ | service_test.go |
| TestComputeNext_InvalidCronExpr | Invalid expression returns nil | ✅ | service_test.go |

---

## Integration Tests

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestBusWithMySQL | Bus + MySQL storage | ✅ | bus_test.go |
| TestBusMessageFlow | Post/claim message flow | ✅ | bus_test.go |
| TestAgentWithBus | Agent + Bus + Tools + Session | ⬜ | |
| TestGatewayWithBus | Gateway + Bus + Mock Channel | ⬜ | |
| TestFullMessageFlow | Gateway → Bus → Agent → Bus → Gateway | ⬜ | |
| TestMultiUserRouting | Multiple users, separate agents | ⬜ | |
| TestOpenAICompatibleProvider | Provider + Real API | ⬜ | Requires API key |

---

## E2E Tests

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestCLI_Version | `--version` flag | ⬜ | |
| TestCLI_Help | `--help` flag | ⬜ | |
| TestCLI_Bus | `nanobus` startup | ⬜ | |
| TestCLI_Gateway | `nanogateway` startup | ⬜ | |
| TestCLI_Agent | `nanosec --user=X` startup | ⬜ | |
| TestCLI_UserAdd | `nanosec user add` command | ⬜ | |
| TestCLI_UserLink | `nanosec user link` command | ⬜ | |
| TestCLI_Onboard | `onboard` command | ⬜ | |
| TestCLI_Status | `status` command | ⬜ | |
| TestCLI_Cron | `cron` commands | ⬜ | |
| TestFullSystem | Bus + Gateway + Agent E2E | ⬜ | |

---

## Concurrency Tests

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestRegistry_ConcurrentAccess | Concurrent tool registry | ⬜ | |
| TestStore_ConcurrentClaim | Concurrent message claiming | ⬜ | |
| TestSession_ConcurrentAccess | Concurrent session access | ⬜ | |
| TestServer_ConcurrentConnections | Multiple WebSocket clients | ⬜ | |
| TestAgent_ConcurrentMessages | Concurrent message processing | ⬜ | |

---

## Security Tests

| Test | Description | Status | Notes |
|------|-------------|--------|-------|
| TestShellTool_PathTraversal | Prevent path traversal | ⬜ | |
| TestFilesystem_PathTraversal | Prevent path traversal | ✅ | filesystem_test.go |
| TestShellTool_CommandInjection | Prevent command injection | ⬜ | |
| TestStore_UserIsolation | Users can only access own queues | ⬜ | |
| TestAgent_UserValidation | Agent validates user_id | ⬜ | |

---

## CI Pipeline Status

| Check | Status | Notes |
|-------|--------|-------|
| Linting (golangci-lint) | ⬜ | |
| Unit Tests | ✅ | |
| Race Detection | ✅ | |
| Coverage Check (80%+) | ⬜ | |
| Integration Tests | ✅ | |
| Build | ⬜ | |

---

## Change Log

| Date | Change |
|------|--------|
| 2026-09-01 | `golangci-lint run ./...` now clean (was 22 errcheck/unused/gosimple findings across agent, session, provider, tool, hypervisor, cmd). Session package passes -race after the syncLoop syncInterval race fix. |
| 2026-09-01 | Empty-completion hardening: 5 sanitizer tests (birth-empty, whitespace-only, wedged-session reproduction), 3 agent loop tests (retry-once-then-fail, retry succeeds, wedged session heals at request time), 2 subagent tests. All pass with -race; provider + agent packages green. |
| 2026-02-22 | Parity sprint: Added 21 new tests — edit_file (5), append_note (4), shell deny list (6), context memory (4). agent coverage 23.1%→41.9%, tool coverage 69.2%→69.7%. |
| 2026-02-22 | Progress audit: corrected all coverage numbers to actual `go test -cover` values. Key corrections: agent 100%→23.1%, provider 100%→26.0%, tool 100%→69.2%, session 100%→89.2%, feishu ~90%→56.6%, wecom ~90%→62.3%. Added skill package tests (13 tests, 81.5%). Overall coverage ~55%, well below 80% target. |
| 2026-02-21 | Sprint 5.1: Added 11 cron service tests (service_test.go), 12 CronTool tests (cron_test.go), 5 SpawnTool tests (spawn_test.go). internal/cron ~85% coverage. |
| 2026-02-12 | Sprint 4.2: Added 20 tests for Feishu/WeCom adapters (10 each), 3 new config tests. gateway/channel coverage ~90% |
| 2026-02-11 | Phase 3 completed: Agent core, LLM provider, Session management, Tool system. 100% coverage in new packages. |
| 2026-02-10 | Phase 2 testing completed: bus handlers (7 tests, 52.9%), bus/store (12 tests, 68.4%), protocol client (10 tests, 68.2%), config updated to 92.3% |
| 2026-02-10 | protocol and config unit tests completed (13 + 16 tests) |
| 2026-02-09 | Updated for multi-user three-process architecture (Gateway, Bus, Agent) |
| 2026-02-09 | Initial test tracking setup |
