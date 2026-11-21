package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// goalCommandPrefix starts a long-running goal from the conversation.
const goalCommandPrefix = "/goal"

// parseGoalCommand reports whether the message is a /goal command and returns
// its argument. "/goals" is not a command.
func parseGoalCommand(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, goalCommandPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(trimmed, goalCommandPrefix)
	if rest == "" {
		return "", true
	}
	// The next character must separate the command from its argument; "/goals"
	// is a different word, not a goal. Decode a rune, not a byte: the full-width
	// colon is three bytes long.
	sep, size := utf8.DecodeRuneInString(rest)
	if !strings.ContainsRune(" \t：:", sep) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimLeft(rest[size:], " \t：:")), true
}

// handleGoalCommand creates a goal from one line of description and answers in
// the same conversation.
//
// The creation flow with its six questions and confirmation card arrives with
// the panel work; this is the command-line path that makes the loop testable
// today. The scheduler reads the goal from the workspace, so the message only
// has to exist there before the next tick.
func (a *Agent) handleGoalCommand(ctx context.Context, msg *nanoprotocol.InboundMessage, description string) {
	reply := func(text string) {
		out := nanoprotocol.OutboundMessage{
			MessageID: messageid.New(),
			AgentID:   a.config.AgentID,
			UserID:    a.config.UserID,
			SessionID: msg.SessionID,
			Channel:   msg.Channel,
			ChatID:    msg.ChatID,
			Content:   text,
		}
		if err := a.busClient.PostOutbound(out); err != nil {
			slog.Warn("post goal command reply", "error", err)
		}
	}

	if description == "" {
		reply("用法：/goal <一句话说明要达成的状态>。例如：/goal internal/auth 下不再出现 OldToken 调用，且 go build ./... 退出 0")
		return
	}
	if a.config.WorkspaceURL == "" {
		reply("这台 agent 没有配置 workspace 地址，无法创建目标。")
		return
	}
	baseKey, _ := session.ParseSessionKey(msg.SessionID)
	body := map[string]any{
		"user_id":     a.config.UserID,
		"agent_id":    a.config.AgentID,
		"session_key": baseKey,
		"channel":     msg.Channel,
		"chat_id":     msg.ChatID,
		"title":       truncateRunes(description, 40),
		"condition":   description,
		"origin":      "manual",
	}
	raw, _ := json.Marshal(body)

	client := &http.Client{Timeout: 10 * time.Second}
	endpoint := strings.TrimRight(a.config.WorkspaceURL, "/") + "/api/v1/workspace/goals"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		reply("创建目标失败：" + err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token := config.InternalToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("create goal failed", "error", err)
		reply("创建目标失败：" + err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		reply(fmt.Sprintf("创建目标失败：workspace 返回 %d", resp.StatusCode))
		return
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		reply("目标已创建，但回执无法解析。")
		return
	}
	reply(fmt.Sprintf("长程目标已创建（%s），第一轮会在几秒内开始。判据：%s", created.ID, truncateRunes(description, 200)))
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
