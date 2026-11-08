package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/mcp"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// File arguments are a convention, not part of MCP: a tool's input schema marks
// a property with x-mcp-file and both ends agree to move the bytes beside the
// JSON-RPC channel. The model then passes a workspace path and never the body.
//
// The shape follows the proposals in the specification repository (SEP-2356 for
// the declaration, SEP-2631 for "the bytes travel out of band, the call carries
// a URI"), neither of which is merged, so the extension is ours to honour — and
// to strip before the provider ever sees it.

// maxFileArgSize bounds a file argument whose schema does not say. It matches
// the artifacts publish ceiling; a larger body is a mistake, not a page.
const maxFileArgSize = 16 << 20

// defaultUploadEndpoint is where a body goes when the schema names no path.
const defaultUploadEndpoint = "/mcp/blobs"

// fileParam is one x-mcp-file declaration.
type fileParam struct {
	Mode     string // "upload" or "download"
	Endpoint string // server-relative, uploads only
	MaxSize  int64
}

// fileParams reads the declarations out of a tool's input schema. Anything
// malformed is ignored: a bad hint must not break the call it describes.
func fileParams(schema json.RawMessage) map[string]fileParam {
	if len(schema) == 0 {
		return nil
	}
	var parsed struct {
		Properties map[string]struct {
			File *struct {
				TransferModes  []string `json:"transferModes"`
				UploadEndpoint string   `json:"uploadEndpoint"`
				MaxSize        int64    `json:"maxSize"`
			} `json:"x-mcp-file"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		return nil
	}
	out := map[string]fileParam{}
	for name, prop := range parsed.Properties {
		if prop.File == nil {
			continue
		}
		mode := ""
		for _, m := range prop.File.TransferModes {
			switch m {
			case "upload":
				mode = "upload"
			case "download":
				if mode == "" {
					mode = "download"
				}
			}
		}
		if mode == "" {
			continue
		}
		out[name] = fileParam{Mode: mode, Endpoint: prop.File.UploadEndpoint, MaxSize: prop.File.MaxSize}
	}
	return out
}

// stripFileParams removes the extension from a schema before it reaches the
// provider. The model should see a plain string property: the declaration is an
// instruction to the runtime, and a model that tried to honour it itself would
// invent data: URIs.
func stripFileParams(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return schema
	}
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return schema
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		return schema
	}
	changed := false
	for _, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := prop["x-mcp-file"]; has {
			delete(prop, "x-mcp-file")
			changed = true
		}
	}
	if !changed {
		return schema
	}
	stripped, err := json.Marshal(doc)
	if err != nil {
		return schema
	}
	return stripped
}

// materializeUploads replaces each upload-declared path with the handle the
// server minted for its bytes.
func (b *McpBridge) materializeUploads(ctx context.Context, client mcp.Client, params map[string]fileParam, args map[string]any) error {
	for name, p := range params {
		if p.Mode != "upload" {
			continue
		}
		raw, ok := args[name]
		if !ok || raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		// Already a handle (a retry after the runtime substituted one, or a
		// client that did its own upload): leave it to the server.
		if strings.Contains(value, "://") {
			continue
		}

		path, err := tool.ResolveWorkspacePath(b.workspace, b.shared, value)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s: cannot read %s: %w", name, value, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: %s is not a regular file", name, value)
		}
		limit := p.MaxSize
		if limit <= 0 {
			limit = maxFileArgSize
		}
		if info.Size() > limit {
			return fmt.Errorf("%s: %s is %d bytes, over the %d-byte limit", name, value, info.Size(), limit)
		}
		if info.Size() == 0 {
			return fmt.Errorf("%s: %s is empty", name, value)
		}

		uploader, ok := client.(mcp.BlobUploader)
		if !ok {
			return fmt.Errorf("%s: this transport cannot upload file arguments", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("%s: open %s: %w", name, value, err)
		}
		endpoint := p.Endpoint
		if endpoint == "" {
			endpoint = defaultUploadEndpoint
		}
		ref, uploadErr := uploader.UploadBlob(ctx, file, info.Size(), endpoint)
		_ = file.Close()
		if uploadErr != nil {
			return fmt.Errorf("%s: upload %s: %w", name, value, uploadErr)
		}
		args[name] = ref.URI
	}
	return nil
}

// materializeDownload turns a download descriptor into a file and replaces the
// tool result with where it went. The body never enters the transcript.
func (b *McpBridge) materializeDownload(ctx context.Context, client mcp.Client, params map[string]fileParam, args map[string]any, result *string) error {
	for name, p := range params {
		if p.Mode != "download" {
			continue
		}
		raw, ok := args[name]
		if !ok || raw == nil {
			continue
		}
		target, ok := raw.(string)
		if !ok || strings.TrimSpace(target) == "" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(*result), &payload); err != nil {
			return nil // not a structured result; nothing to do
		}
		descriptor, ok := payload["download"].(map[string]any)
		if !ok {
			return nil
		}
		rawPath, _ := descriptor["raw_path"].(string)
		if rawPath == "" {
			return fmt.Errorf("%s: the server returned a download without a path", name)
		}
		declared, _ := descriptor["size_bytes"].(float64)
		limit := p.MaxSize
		if limit <= 0 {
			limit = maxFileArgSize
		}
		if declared > float64(limit) {
			return fmt.Errorf("%s: %s is %.0f bytes, over the %d-byte limit", name, target, declared, limit)
		}

		downloader, ok := client.(mcp.BlobUploader)
		if !ok {
			return fmt.Errorf("%s: this transport cannot download file arguments", name)
		}
		path, err := tool.ResolveWorkspacePath(b.workspace, b.shared, target)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("%s: create directory: %w", name, err)
		}

		body, _, err := downloader.OpenBlob(ctx, rawPath)
		if err != nil {
			return fmt.Errorf("%s: download: %w", name, err)
		}
		written, hash, err := writeLimited(path, body, limit)
		_ = body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if want, _ := descriptor["content_hash"].(string); want != "" && want != hash {
			_ = os.Remove(path)
			return fmt.Errorf("%s: downloaded %d bytes whose hash is %s, not the %s the server declared", name, written, hash, want)
		}

		delete(payload, "download")
		payload["saved_to"] = target
		payload["size_bytes"] = written
		payload["content_hash"] = hash
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%s: encode result: %w", name, err)
		}
		*result = string(encoded)
	}
	return nil
}

// writeLimited streams body into path atomically: a partial download must not
// leave a truncated file where the model will read it.
func writeLimited(path string, body io.Reader, limit int64) (int64, string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp-download-")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}

	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, limit+1))
	if err != nil {
		cleanup()
		return 0, "", fmt.Errorf("write download: %w", err)
	}
	if written > limit {
		cleanup()
		return 0, "", fmt.Errorf("download exceeds the %d-byte limit", limit)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return 0, "", fmt.Errorf("close download: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return 0, "", fmt.Errorf("save download: %w", err)
	}
	return written, hex.EncodeToString(sum.Sum(nil)), nil
}
