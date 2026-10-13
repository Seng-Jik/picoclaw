package tools

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

const (
	largeBase64OmittedMessage = toolshared.LargeBase64OmittedMessage
	inlineMediaOmittedMessage = toolshared.InlineMediaOmittedMessage
	inlineMediaStoredMessage  = "[Tool returned inline media content (%s); omitted from model context and registered as a media attachment.]"
)

var (
	inlineMarkdownDataURLRe = regexp.MustCompile(`!\[[^\]]*\]\((data:[^)]+)\)`)
	inlineRawDataURLRe      = regexp.MustCompile(`data:[^;\s]+;base64,[A-Za-z0-9+/=\r\n]+`)
)

func normalizeToolResult(
	result *ToolResult,
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
	workspace string,
	maxInlineResultRunes int,
) *ToolResult {
	if result == nil {
		return nil
	}

	// Work on a shallow copy so the tool-owned result is never mutated in
	// place. A tool that returns a cached/shared *ToolResult would otherwise
	// accumulate media refs and re-emit them on subsequent calls.
	normalized := *result
	if len(result.Media) > 0 {
		normalized.Media = append([]string(nil), result.Media...)
	}
	if len(result.ArtifactTags) > 0 {
		normalized.ArtifactTags = append([]string(nil), result.ArtifactTags...)
	}
	result = &normalized

	notes := make([]string, 0, 2)
	seen := make(map[string]struct{})

	if store != nil && channel != "" && chatID != "" {
		var refs []string
		var extractedNotes []string

		result.ForLLM, refs, extractedNotes = extractInlineMediaRefs(
			result.ForLLM,
			toolName,
			store,
			channel,
			chatID,
			seen,
		)
		result.Media = append(result.Media, refs...)
		notes = append(notes, extractedNotes...)

		result.ForUser, refs, extractedNotes = extractInlineMediaRefs(
			result.ForUser,
			toolName,
			store,
			channel,
			chatID,
			seen,
		)
		result.Media = append(result.Media, refs...)
		notes = append(notes, extractedNotes...)
	}

	result.ForLLM = toolshared.SanitizeToolLLMContent(result.ForLLM)

	if len(result.Media) > 0 && len(notes) > 0 {
		if strings.TrimSpace(result.ForLLM) == "" {
			result.ForLLM = strings.Join(notes, "\n")
		} else {
			result.ForLLM = strings.TrimSpace(result.ForLLM) + "\n" + strings.Join(notes, "\n")
		}
	}
	if len(result.Media) > 0 && strings.TrimSpace(result.ForLLM) == "" {
		result.ForLLM = "[Tool returned media content; omitted from model context and registered as a media attachment.]"
	}

	persistLargeResultArtifact(result, toolName, workspace, maxInlineResultRunes)

	return result
}

// persistLargeResultArtifact spills an oversized tool result to a workspace
// artifact so the full payload does not consume model context. It mirrors the
// large-text handling used for MCP tools: ForLLM is replaced with a short note
// and the artifact path is exposed via ArtifactTags. It is a no-op when the
// result is small, the workspace is unknown, or persistence is disabled.
func persistLargeResultArtifact(result *ToolResult, toolName, workspace string, maxRunes int) {
	if result == nil || maxRunes <= 0 || strings.TrimSpace(workspace) == "" {
		return
	}
	if result.ResponseHandled {
		// The tool already delivered its payload to the user; the model only
		// needs the short confirmation.
		return
	}

	text := strings.TrimSpace(result.ForLLM)
	size := utf8.RuneCountInString(text)
	if text == "" || size <= maxRunes {
		return
	}

	dir := filepath.Join(workspace, ".artifacts", "tool")
	pattern := sanitizeIdentifierComponent(toolName) + "_*.txt"
	path, err := toolshared.PersistTextArtifact(dir, pattern, text)
	if err != nil {
		logger.WarnCF("tool", "Failed to persist large tool result artifact", map[string]any{
			"tool":  toolName,
			"chars": size,
			"error": err.Error(),
		})
		result.ForLLM = fmt.Sprintf(
			"[Tool returned a large result (%d chars); omitted from model context because artifact persistence failed.]",
			size,
		)
		return
	}

	result.ForLLM = fmt.Sprintf(
		"[Tool returned a large result (%d chars); omitted from model context and saved as a local artifact.]",
		size,
	)
	result.ArtifactTags = append(result.ArtifactTags, "[file:"+path+"]")
}

func extractInlineMediaRefs(
	text string,
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
	seen map[string]struct{},
) (cleaned string, refs []string, notes []string) {
	cleaned = text

	matches := inlineMarkdownDataURLRe.FindAllStringSubmatch(cleaned, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		dataURL := match[1]
		ref, note := storeInlineDataURL(toolName, store, channel, chatID, dataURL, seen)
		if ref != "" {
			refs = append(refs, ref)
		}
		if note != "" {
			notes = append(notes, note)
		}
		cleaned = strings.ReplaceAll(cleaned, match[0], "")
	}

	rawMatches := inlineRawDataURLRe.FindAllString(cleaned, -1)
	for _, dataURL := range rawMatches {
		ref, note := storeInlineDataURL(toolName, store, channel, chatID, dataURL, seen)
		if ref != "" {
			refs = append(refs, ref)
		}
		if note != "" {
			notes = append(notes, note)
		}
		cleaned = strings.ReplaceAll(cleaned, dataURL, "")
	}

	return strings.TrimSpace(cleaned), refs, notes
}

func storeInlineDataURL(
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
	dataURL string,
	seen map[string]struct{},
) (ref string, note string) {
	dataURL = strings.TrimSpace(dataURL)
	if _, ok := seen[dataURL]; ok {
		return "", ""
	}
	seen[dataURL] = struct{}{}

	if !strings.HasPrefix(strings.ToLower(dataURL), "data:") {
		return "", ""
	}

	comma := strings.IndexByte(dataURL, ',')
	if comma <= 5 {
		return "", "[Tool returned inline media content that could not be parsed.]"
	}

	metaPart := dataURL[:comma]
	payload := dataURL[comma+1:]
	if !strings.Contains(strings.ToLower(metaPart), ";base64") {
		return "", "[Tool returned inline media content that was not base64-encoded.]"
	}

	mimeType := strings.TrimSpace(strings.TrimPrefix(metaPart, "data:"))
	if semi := strings.IndexByte(mimeType, ';'); semi >= 0 {
		mimeType = mimeType[:semi]
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	payload = strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(payload)
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Sprintf("[Tool returned inline media content (%s) that could not be decoded.]", mimeType)
	}

	dir := media.TempDir()
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Sprintf("[Tool returned inline media content (%s) but it could not be stored.]", mimeType)
	}

	ext := toolshared.ExtensionForMIMEType(mimeType)
	tmpFile, err := os.CreateTemp(dir, "tool-inline-*"+ext)
	if err != nil {
		return "", fmt.Sprintf("[Tool returned inline media content (%s) but it could not be stored.]", mimeType)
	}
	tmpPath := tmpFile.Name()
	if _, err = tmpFile.Write(decoded); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[Tool returned inline media content (%s) but it could not be stored.]", mimeType)
	}
	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[Tool returned inline media content (%s) but it could not be stored.]", mimeType)
	}

	filename := sanitizeIdentifierComponent(toolName) + ext
	scope := fmt.Sprintf(
		"tool:inline:%s:%s:%s:%d",
		sanitizeIdentifierComponent(toolName),
		channel,
		chatID,
		time.Now().UnixNano(),
	)

	ref, err = store.Store(tmpPath, media.MediaMeta{
		Filename:    filename,
		ContentType: mimeType,
		Source:      fmt.Sprintf("tool:inline:%s", sanitizeIdentifierComponent(toolName)),
	}, scope)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[Tool returned inline media content (%s) but it could not be registered.]", mimeType)
	}

	return ref, fmt.Sprintf(inlineMediaStoredMessage, mimeType)
}
