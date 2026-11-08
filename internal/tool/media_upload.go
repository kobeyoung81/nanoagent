package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// MediaUploadTool uploads a local file to the WeCom temporary media API and
// immediately delivers it to the current (or specified) chat via the bus.
type MediaUploadTool struct {
	BaseDir    string // base directory for media files
	Workspace  string // agent workspace directory
	CorpID     string
	CorpSecret string
	AgentID    int64
	Client     BusClient
	UserID     string

	mu      sync.Mutex
	channel string
	chatID  string

	tokenFn func(ctx context.Context) (string, error)
}

// NewMediaUploadTool creates a new media upload tool.
func NewMediaUploadTool(baseDir, workspace, corpID, corpSecret string, agentID int64, client BusClient, userID string) *MediaUploadTool {
	t := &MediaUploadTool{
		BaseDir:    baseDir,
		Workspace:  workspace,
		CorpID:     corpID,
		CorpSecret: corpSecret,
		AgentID:    agentID,
		Client:     client,
		UserID:     userID,
	}
	t.tokenFn = t.fetchToken
	return t
}

// SetContext implements ContextualTool so the registry can inject the current
// session's channel and chatID before each tool call.
func (t *MediaUploadTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.channel = channel
	t.chatID = chatID
}

func (t *MediaUploadTool) Name() string { return "upload_media" }
func (t *MediaUploadTool) Description() string {
	return "Upload a local file to WeCom and send it to the current chat (or a specified channel/chat). " +
		"Returns upload metadata including media_id."
}

func (t *MediaUploadTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"source_path": {Type: "string", Description: "Path to the file to upload (relative to workspace or absolute)"},
			"channel":     {Type: "string", Description: "Channel to send the file to (optional, defaults to current session channel)"},
			"chat_id":     {Type: "string", Description: "Chat ID to send the file to (optional, defaults to current session chat)"},
		},
		Required: []string{"source_path"},
	}
}

func (t *MediaUploadTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	sourcePath, ok := params["source_path"].(string)
	if !ok || sourcePath == "" {
		return "", fmt.Errorf("missing source_path")
	}

	// Resolve path relative to workspace if not absolute
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(t.Workspace, sourcePath)
	}

	// Security: ensure path is within workspace or media directory
	absPath, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	absWorkspace, _ := filepath.Abs(t.Workspace)
	absBaseDir, _ := filepath.Abs(t.BaseDir)
	if !strings.HasPrefix(absPath, absWorkspace) && !strings.HasPrefix(absPath, absBaseDir) {
		return "", fmt.Errorf("access denied: path must be within workspace or media directory")
	}

	// Read file
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	filename := filepath.Base(absPath)
	mimeType := guessMimeType(filename)

	// Store in media directory with hash
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	mediaDir := filepath.Join(t.BaseDir, hashStr)
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}

	localPath := filepath.Join(mediaDir, filename)
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return "", fmt.Errorf("write to media dir: %w", err)
	}

	// Determine WeCom media type
	wecomType := classifyMediaType(mimeType)

	// Upload to WeCom temp media API
	token, err := t.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	mediaID, err := t.uploadToWeCom(ctx, token, wecomType, filename, data)
	if err != nil {
		return "", fmt.Errorf("upload to wecom: %w", err)
	}

	// Determine target channel and chatID (params override session context)
	t.mu.Lock()
	targetChannel := t.channel
	targetChatID := t.chatID
	t.mu.Unlock()

	if ch, ok := params["channel"].(string); ok && ch != "" {
		targetChannel = ch
	}
	if cid, ok := params["chat_id"].(string); ok && cid != "" {
		targetChatID = cid
	}

	// Send the media via the bus immediately
	if targetChannel != "" && targetChatID != "" {
		outbound := nanoprotocol.OutboundMessage{
			UserID:    t.UserID,
			SessionID: fmt.Sprintf("%s:%s", targetChannel, targetChatID),
			Channel:   targetChannel,
			ChatID:    targetChatID,
			MessageID: messageid.New(),
			Media: []nanoprotocol.MediaItem{
				{
					Platform: "wecom",
					MediaID:  mediaID,
					Filename: filename,
					MimeType: mimeType,
				},
			},
		}
		if err := t.Client.PostOutbound(outbound); err != nil {
			return "", fmt.Errorf("send media: %w", err)
		}
	}

	result := map[string]any{
		"platform":   "wecom",
		"media_id":   mediaID,
		"filename":   filename,
		"mime_type":  mimeType,
		"local_path": localPath,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

func (t *MediaUploadTool) uploadToWeCom(ctx context.Context, token, mediaType, filename string, data []byte) (string, error) {
	url := fmt.Sprintf(
		"https://qyapi.weixin.qq.com/cgi-bin/media/upload?access_token=%s&type=%s",
		token, mediaType,
	)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("media", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("write form file: %w", err)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode  int    `json:"errcode"`
		ErrMsg   string `json:"errmsg"`
		Type     string `json:"type"`
		MediaID  string `json:"media_id"`
		CreateAt string `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("upload error: %d %s", result.ErrCode, result.ErrMsg)
	}

	return result.MediaID, nil
}

func (t *MediaUploadTool) fetchToken(ctx context.Context) (string, error) {
	url := fmt.Sprintf(
		"https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		t.CorpID, t.CorpSecret,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("token error: %d %s", result.ErrCode, result.ErrMsg)
	}
	return result.AccessToken, nil
}

func classifyMediaType(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "audio/"):
		return "voice"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	default:
		return "file"
	}
}
