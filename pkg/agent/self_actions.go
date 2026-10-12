package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	maxSelfLogLines   = 200
	maxSelfLogBytes   = int64(256 << 10)
	maxSelfListValues = 50
)

// executeList enumerates the children of a snapshot node together with the
// access level and numeric range the self tool applies to each child.
func (t *SelfTool) executeList(ctx context.Context, key string) *tools.ToolResult {
	view, errResult := t.runtimeView(ctx)
	if errResult != nil {
		return errResult
	}
	var node any = view
	if key != "" {
		if err := validateSelfPath(key); err != nil {
			t.audit(ctx, "list", "BLOCKED "+key)
			return tools.ErrorResult(err.Error())
		}
		var err error
		node, err = resolveSelfPath(view, key)
		if err != nil {
			t.audit(ctx, "list", "REJECTED "+key)
			return tools.ErrorResult(err.Error())
		}
	}
	encoded, err := json.Marshal(selfListChildren(node, key))
	if err != nil {
		return tools.ErrorResult("list is not JSON-compatible")
	}
	t.audit(ctx, "list", key)
	return tools.NewToolResult(string(encoded))
}

func selfListChildren(node any, path string) map[string]any {
	out := map[string]any{}
	switch typed := node.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			childPath := name
			if path != "" {
				childPath = path + "." + name
			}
			info := map[string]any{"access": selfAccessLevelName(selfAccessLevelFor(childPath))}
			if rule, ok := selfAccessRuleFor(childPath); ok {
				if label := selfRangeLabel(rule); label != "" {
					info["range"] = label
				}
			}
			out[name] = info
		}
	case []any:
		out["type"] = "array"
		out["length"] = len(typed)
		out["access"] = selfAccessLevelName(selfAccessLevelFor(path))
	case []string:
		out["type"] = "array"
		out["length"] = len(typed)
		if len(typed) <= maxSelfListValues {
			out["values"] = typed
		}
	default:
		out["type"] = fmt.Sprintf("%T", typed)
		out["value"] = formatSelfValue(typed)
		out["access"] = selfAccessLevelName(selfAccessLevelFor(path))
	}
	return out
}

// executeSchema prints the whole access table so the agent can learn which
// paths are readable, writable and within which range.
func (t *SelfTool) executeSchema(ctx context.Context) *tools.ToolResult {
	entries := make([]map[string]any, 0, len(selfAccessRules))
	for _, rule := range selfAccessRules {
		entry := map[string]any{
			"path":   rule.path,
			"access": selfAccessLevelName(rule.level),
		}
		if label := selfRangeLabel(rule); label != "" {
			entry["range"] = label
		}
		entries = append(entries, entry)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return tools.ErrorResult("schema is not JSON-compatible")
	}
	t.audit(ctx, "schema", fmt.Sprintf("%d rules", len(entries)))
	return tools.NewToolResult(string(encoded))
}

// executeExplain describes a single path: its access level, whether it is
// writable, and its current (redacted) value when readable.
func (t *SelfTool) executeExplain(ctx context.Context, key string) *tools.ToolResult {
	if key == "" {
		t.audit(ctx, "explain", "REJECTED empty key")
		return tools.ErrorResult("explain requires a key")
	}
	if selfPathDenied(key) {
		t.audit(ctx, "explain", "BLOCKED "+key)
		return tools.ErrorResult("access denied")
	}
	for _, part := range strings.Split(key, ".") {
		if selfSensitiveName(part) {
			t.audit(ctx, "explain", "BLOCKED "+key)
			return tools.ErrorResult("access denied")
		}
	}
	level := selfAccessLevelFor(key)
	result := map[string]any{
		"path":     key,
		"access":   selfAccessLevelName(level),
		"writable": level == selfAccessRestricted && !strings.Contains(key, ".") && selfRuntimeRoots[key],
	}
	if rule, ok := selfAccessRuleFor(key); ok {
		if label := selfRangeLabel(rule); label != "" {
			result["range"] = label
		}
	}
	if description := selfFieldDescription(key); description != "" {
		result["description"] = description
	}
	if level != selfAccessBlocked && t.runtime != nil {
		view, _ := t.runtimeView(ctx)
		if value, err := resolveSelfPath(view, key); err == nil {
			result["value"] = value
		} else if note, ok := t.getNote(ctx, key); ok {
			result["value"] = note
		} else {
			result["value"] = "<not found>"
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return tools.ErrorResult("explain is not JSON-compatible")
	}
	t.audit(ctx, "explain", key)
	return tools.NewToolResult(string(encoded))
}

// selfFieldDescription returns a short human description for well-known paths.
func selfFieldDescription(key string) string {
	switch key {
	case "max_iterations":
		return "Maximum tool-iteration count for this session (1-100; effective immediately)."
	case "context_window_tokens":
		return "Session context window in tokens (4096-1000000; effective immediately)."
	case "model":
		return "Current model name; read-only. Use model_preset to switch."
	case "model_preset":
		return "Configured model preset selected for this session (effective next turn)."
	case "workspace":
		return "Agent workspace directory; read-only."
	case "tool_names":
		return "Tools available to this agent; read-only."
	case "request":
		return "Request metadata (channel, chat_id, sender_id); read-only."
	case "web_config":
		return "Web tool settings (enabled, provider, format); read-only."
	case "exec_config":
		return "Exec tool settings; read-only."
	case "model_presets":
		return "Configured model preset names; read-only."
	case "defaults":
		return "Configured agent defaults (max_iterations, context_window_tokens, model); read-only."
	case "session_overrides":
		return "Session-scoped values that differ from the configured defaults; read-only."
	case "config":
		return "Full PicoClaw configuration with credentials removed; read-only."
	case "scratchpad":
		return "Session-only notes set with the self tool."
	case "logs":
		return "PicoClaw's log files, read with the logs action."
	}
	if selfIsConfigPath(key) {
		return "PicoClaw configuration field (read-only; credentials redacted)."
	}
	return ""
}

// executeDiff compares the session overrides with the configured defaults.
func (t *SelfTool) executeDiff(ctx context.Context) *tools.ToolResult {
	view, errResult := t.runtimeView(ctx)
	if errResult != nil {
		return errResult
	}
	defaults, _ := view["defaults"].(map[string]any)
	overrides, _ := view["session_overrides"].(map[string]any)
	if len(overrides) == 0 {
		t.audit(ctx, "diff", "clean")
		return tools.NewToolResult("no session overrides; using the configured defaults")
	}
	result := make(map[string]any, len(overrides))
	for key, sessionValue := range overrides {
		defaultKey := key
		if key == "model_preset" {
			defaultKey = "model"
		}
		result[key] = map[string]any{
			"default": defaults[defaultKey],
			"session": sessionValue,
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return tools.ErrorResult("diff is not JSON-compatible")
	}
	t.audit(ctx, "diff", fmt.Sprintf("%d overrides", len(overrides)))
	return tools.NewToolResult(string(encoded))
}

// executeLogs tails PicoClaw's own log files, filtering by level or substring
// and redacting credential-like values.
func (t *SelfTool) executeLogs(ctx context.Context, key string) *tools.ToolResult {
	files := logger.ActiveLogFiles()
	if len(files) == 0 {
		t.audit(ctx, "logs", "REJECTED no file logging")
		return tools.ErrorResult(
			"file logging is not enabled; set PICOCLAW_LOG_FILE or start the gateway",
		)
	}
	lines := make([]string, 0, maxSelfLogLines*2)
	for i := len(files) - 1; i >= 0; i-- {
		tail, err := readSelfFileTail(files[i], maxSelfLogBytes)
		if err != nil {
			continue
		}
		lines = append(lines, strings.Split(strings.TrimRight(tail, "\n"), "\n")...)
	}
	levelFilter, filterIsLevel := parseSelfLevelFilter(key)
	matched := make([]string, 0, maxSelfLogLines)
	for _, line := range lines {
		if line == "" {
			continue
		}
		summary, level := selfLogSummary(line)
		if filterIsLevel && level != "" && levelRank(level) < levelRank(levelFilter) {
			continue
		}
		if !filterIsLevel && key != "" &&
			!strings.Contains(strings.ToLower(summary), strings.ToLower(key)) {
			continue
		}
		matched = append(matched, summary)
	}
	if len(matched) > maxSelfLogLines {
		matched = matched[len(matched)-maxSelfLogLines:]
	}
	t.audit(ctx, "logs", fmt.Sprintf("%d lines", len(matched)))
	if len(matched) == 0 {
		return tools.NewToolResult("no log lines matched")
	}
	return tools.NewToolResult(strings.Join(matched, "\n"))
}

// selfLogSummary redacts a raw log line and reduces it to the fields worth
// showing. Non-JSON lines (for example panic traces) are scrubbed as text.
func selfLogSummary(line string) (string, string) {
	line = strings.TrimSpace(line)
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return scrubSelfLogText(line), ""
	}
	redacted, _ := redactSelfConfigValue("", obj).(map[string]any)
	if redacted == nil {
		return scrubSelfLogText(line), ""
	}
	summary := map[string]any{}
	for _, name := range []string{"time", "level", "component", "message", "error"} {
		if value, ok := redacted[name]; ok {
			summary[name] = value
		}
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return scrubSelfLogText(line), ""
	}
	level, _ := summary["level"].(string)
	return string(encoded), strings.ToLower(level)
}

var selfLogSecretPattern = regexp.MustCompile(
	`(?i)(api[_-]?key|secret|password|passphrase|token|credential|authorization|bearer)` +
		`(\s*[:=]\s*"?)([^\s"',}]+)`,
)

func scrubSelfLogText(line string) string {
	return selfLogSecretPattern.ReplaceAllString(line, "${1}${2}[REDACTED]")
}

func parseSelfLevelFilter(key string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "debug", "info", "warn", "warning", "error", "fatal":
		return strings.ToLower(strings.TrimSpace(key)), true
	default:
		return "", false
	}
}

func levelRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "info":
		return 1
	case "warn", "warning":
		return 2
	case "error":
		return 3
	case "fatal":
		return 4
	default:
		return 1
	}
}

// readSelfFileTail reads up to maxBytes from the end of path and trims a
// partial leading line.
func readSelfFileTail(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	text := string(data)
	if start > 0 {
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		}
	}
	return text, nil
}
