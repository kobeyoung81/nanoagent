package agent

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

const (
	// defaultVisionMaxImageBytes caps one inlined image (raw bytes).
	defaultVisionMaxImageBytes = 8 << 20
	// defaultVisionMaxImagesPerMessage caps attachments per user turn.
	defaultVisionMaxImagesPerMessage = 4
)

// allowedImageMIME lists the image types inlined for vision models.
var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// mediaNote renders a short, model-readable description of an attachment,
// including its workspace path so the agent can act on it with file tools.
func mediaNote(item nanoprotocol.MediaItem) string {
	name := item.Filename
	if name == "" {
		name = item.MediaID
	}
	mime := item.MimeType
	if mime == "" {
		mime = "unknown type"
	}
	if item.URL != "" {
		return fmt.Sprintf("[Received file: %q (%s) at %s]", name, mime, item.URL)
	}
	return fmt.Sprintf("[Received file: %q (%s)]", name, mime)
}

// imageRefFromMedia converts an inbound media item into a local image
// reference when it names an image in the agent/shared workspace. Non-image or
// non-local media yields false and stays a text note.
func imageRefFromMedia(item nanoprotocol.MediaItem) (provider.ImageRef, bool) {
	if item.MimeType != "" && !strings.HasPrefix(item.MimeType, "image/") {
		return provider.ImageRef{}, false
	}
	p := strings.TrimPrefix(item.URL, "file://")
	if p == "" {
		return provider.ImageRef{}, false
	}
	return provider.ImageRef{Path: p, MimeType: item.MimeType}, true
}

// expandImages converts persisted ImageRefs into wire-ready content parts,
// reading and base64-encoding each file just before a provider call. It is
// best-effort: unreadable, oversized or disallowed attachments are skipped
// (with a log) so a bad file never wedges the turn. The returned messages are
// shallow copies; the caller's session is never mutated with base64.
func (a *Agent) expandImages(msgs []provider.Message) []provider.Message {
	maxBytes := a.config.VisionMaxImageBytes
	if maxBytes <= 0 {
		maxBytes = defaultVisionMaxImageBytes
	}
	maxImages := a.config.VisionMaxImagesPerMessage
	if maxImages <= 0 {
		maxImages = defaultVisionMaxImagesPerMessage
	}

	var out []provider.Message
	for i, m := range msgs {
		if len(m.Images) == 0 {
			continue
		}
		existingText := m.Content
		// A message expanded in an earlier iteration already has parts.
		if len(m.Parts) > 0 {
			continue
		}
		parts := make([]provider.ContentPart, 0, len(m.Images)+1)
		if strings.TrimSpace(existingText) != "" {
			parts = append(parts, provider.ContentPart{Type: "text", Text: existingText})
		}
		added := 0
		for _, ref := range m.Images {
			if added >= maxImages {
				slog.Warn("vision: per-message image limit reached", "limit", maxImages)
				break
			}
			dataURL, err := a.imageDataURL(ref, maxBytes)
			if err != nil {
				slog.Warn("vision: skipping image", "path", ref.Path, "error", err)
				continue
			}
			parts = append(parts, provider.ContentPart{Type: "image_url", ImageURL: dataURL})
			added++
		}
		if added == 0 {
			continue
		}
		if out == nil {
			out = append([]provider.Message(nil), msgs...)
		}
		out[i].Parts = parts
	}
	if out == nil {
		return msgs
	}
	return out
}

// imageDataURL reads one image reference within the workspace roots and returns
// an inline data: URL.
func (a *Agent) imageDataURL(ref provider.ImageRef, maxBytes int) (string, error) {
	path, err := a.resolveImagePath(ref.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("is a directory")
	}
	if info.Size() > int64(maxBytes) {
		return "", fmt.Errorf("image is %d bytes, exceeds max %d", info.Size(), maxBytes)
	}
	mimeType := ref.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(path))
	}
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	if !allowedImageMIME[mimeType] {
		return "", fmt.Errorf("unsupported image type %q", mimeType)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// resolveImagePath resolves a workspace-relative image path, rejecting any
// escape from the agent/shared workspace roots.
func (a *Agent) resolveImagePath(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed")
	}
	for _, root := range []string{a.config.WorkspaceDir, a.config.SharedWorkspaceDir} {
		if root == "" {
			continue
		}
		abs := filepath.Join(root, rel)
		if withinRoot(abs, root) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the agent and shared workspaces", rel)
}

// withinRoot reports whether abs resolves inside root.
func withinRoot(abs, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(abs))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
