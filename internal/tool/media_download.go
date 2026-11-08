package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// MediaDownloadTool downloads a file from WeCom temporary media API by media_id.
type MediaDownloadTool struct {
	BaseDir    string // base directory for storing media files
	CorpID     string
	CorpSecret string
	AgentID    int64

	tokenFn func(ctx context.Context) (string, error)
}

// NewMediaDownloadTool creates a new media download tool.
func NewMediaDownloadTool(baseDir, corpID, corpSecret string, agentID int64) *MediaDownloadTool {
	t := &MediaDownloadTool{
		BaseDir:    baseDir,
		CorpID:     corpID,
		CorpSecret: corpSecret,
		AgentID:    agentID,
	}
	t.tokenFn = t.fetchToken
	return t
}

func (t *MediaDownloadTool) Name() string { return "download_media" }
func (t *MediaDownloadTool) Description() string {
	return "Download a file from the WeCom media API by media_id. Saves to local storage and returns the local path."
}

func (t *MediaDownloadTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"platform": {Type: "string", Description: "The platform the media belongs to (wecom, wechatkf)"},
			"media_id": {Type: "string", Description: "The media ID to download"},
			"filename": {Type: "string", Description: "Optional filename for the downloaded file"},
		},
		Required: []string{"platform", "media_id"},
	}
}

func (t *MediaDownloadTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	mediaID, ok := params["media_id"].(string)
	if !ok || mediaID == "" {
		return "", fmt.Errorf("missing media_id")
	}

	filename, _ := params["filename"].(string)
	if filename == "" {
		filename = "media"
	}

	token, err := t.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	// Download from WeCom media API
	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/media/get?access_token=%s&media_id=%s", token, mediaID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Check for JSON error response
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api error: %s", string(body))
	}

	// Extract filename from Content-Disposition if available
	if cd := resp.Header.Get("Content-Disposition"); cd != "" && filename == "media" {
		if idx := strings.Index(cd, "filename=\""); idx >= 0 {
			fn := cd[idx+10:]
			if endIdx := strings.Index(fn, "\""); endIdx >= 0 {
				filename = fn[:endIdx]
			}
		}
	}

	// Detect MIME type
	mimeType := contentType
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = guessMimeType(filename)
	}

	// Read body and compute hash
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	// Save to {mediaBaseDir}/{hash}/{filename}
	dir := filepath.Join(t.BaseDir, hashStr)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}

	localPath := filepath.Join(dir, filename)
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	result := map[string]any{
		"local_path": localPath,
		"filename":   filename,
		"mime_type":  mimeType,
		"size":       len(data),
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

func (t *MediaDownloadTool) fetchToken(ctx context.Context) (string, error) {
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

func guessMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".amr":
		return "audio/amr"
	case ".pdf":
		return "application/pdf"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	default:
		return "application/octet-stream"
	}
}
