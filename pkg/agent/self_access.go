package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
)

// selfAccessLevel is the isolation level applied to a self-tool path.
type selfAccessLevel int

const (
	// selfAccessDefault means the path is not covered by the table. Reads are
	// allowed; writes fall through to the session scratchpad.
	selfAccessDefault selfAccessLevel = iota
	// selfAccessBlocked denies both read and write; the value is dropped from
	// the snapshot entirely.
	selfAccessBlocked
	// selfAccessReadOnly allows reads but never writes.
	selfAccessReadOnly
	// selfAccessRestricted allows reads and session-scoped writes that pass the
	// numeric range (when one is declared).
	selfAccessRestricted
)

// selfAccessRule binds a path prefix to an isolation level. Longer prefixes win
// so a BLOCKED sub-section can override a READ_ONLY parent. Numeric slice
// indices are ignored when matching, so config.agents.list[0].subagents matches
// config.agents.list.subagents.
type selfAccessRule struct {
	path  string
	level selfAccessLevel
	min   int
	max   int
}

const (
	selfContextWindowMin = 4096
	selfContextWindowMax = 1000000
)

var selfAccessRules = []selfAccessRule{
	// Runtime / snapshot keys.
	{path: "max_iterations", level: selfAccessRestricted, min: 1, max: 100},
	{
		path: "context_window_tokens", level: selfAccessRestricted,
		min: selfContextWindowMin, max: selfContextWindowMax,
	},
	{path: "model", level: selfAccessReadOnly},
	{path: "model_preset", level: selfAccessRestricted},
	{path: "workspace", level: selfAccessReadOnly},
	{path: "tool_names", level: selfAccessReadOnly},
	{path: "request", level: selfAccessReadOnly},
	{path: "turn", level: selfAccessReadOnly},
	{path: "context_usage", level: selfAccessReadOnly},
	{path: "web_config", level: selfAccessReadOnly},
	{path: "exec_config", level: selfAccessReadOnly},
	{path: "model_presets", level: selfAccessReadOnly},
	{path: "defaults", level: selfAccessReadOnly},
	{path: "session_overrides", level: selfAccessReadOnly},

	// Config sections are allowlisted: an unknown key added to the config is
	// BLOCKED by default (see selfAccessLevelFor). BLOCKED rules below win over
	// their parent section by longest match.
	{path: "config.version", level: selfAccessReadOnly},
	{path: "config.build_info", level: selfAccessReadOnly},
	{path: "config.agents", level: selfAccessReadOnly},
	{path: "config.session", level: selfAccessReadOnly},
	{path: "config.channel_list", level: selfAccessReadOnly},
	{path: "config.model_list", level: selfAccessReadOnly},
	{path: "config.events", level: selfAccessReadOnly},
	{path: "config.heartbeat", level: selfAccessReadOnly},
	{path: "config.power", level: selfAccessReadOnly},
	{path: "config.devices", level: selfAccessReadOnly},
	{path: "config.voice", level: selfAccessReadOnly},
	{path: "config.tools", level: selfAccessReadOnly},

	// Blocked config sections.
	{path: "config.evolution", level: selfAccessBlocked},
	{path: "config.plugins", level: selfAccessBlocked},
	{path: "config.hooks", level: selfAccessBlocked},
	{path: "config.isolation", level: selfAccessBlocked},
	{path: "config.gateway", level: selfAccessBlocked},
	{path: "config.agents.dispatch", level: selfAccessBlocked},
	{path: "config.agents.list.subagents", level: selfAccessBlocked},
	{path: "config.tools.mcp.servers", level: selfAccessBlocked},
	{path: "config.tools.allow_read_paths", level: selfAccessBlocked},
	{path: "config.tools.allow_write_paths", level: selfAccessBlocked},
	{path: "config.tools.filter_sensitive_data", level: selfAccessBlocked},
	{path: "config.tools.filter_min_length", level: selfAccessBlocked},
	{path: "config.agents.defaults.restrict_to_workspace", level: selfAccessBlocked},
	{path: "config.agents.defaults.allow_read_outside_workspace", level: selfAccessBlocked},
	{path: "config.session.identity_links", level: selfAccessBlocked},
	{path: "config.session.dimensions", level: selfAccessBlocked},
	{path: "config.session.dm_scope", level: selfAccessBlocked},
}

// selfSignificantSegments splits a dot path and drops empty and numeric
// segments so rule prefixes match slice elements regardless of index.
func selfSignificantSegments(path string) []string {
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if _, err := strconv.Atoi(part); err == nil {
			continue
		}
		segments = append(segments, part)
	}
	return segments
}

func selfSegmentsHavePrefix(segments, prefix []string) bool {
	if len(prefix) > len(segments) {
		return false
	}
	for i := range prefix {
		if segments[i] != prefix[i] {
			return false
		}
	}
	return true
}

// selfAccessRuleFor returns the most specific rule covering path.
func selfAccessRuleFor(path string) (selfAccessRule, bool) {
	segments := selfSignificantSegments(path)
	if len(segments) == 0 {
		return selfAccessRule{}, false
	}
	var best selfAccessRule
	bestLen := -1
	for _, rule := range selfAccessRules {
		ruleSegments := selfSignificantSegments(rule.path)
		if len(ruleSegments) > len(segments) || len(ruleSegments) <= bestLen {
			continue
		}
		if selfSegmentsHavePrefix(segments, ruleSegments) {
			best, bestLen = rule, len(ruleSegments)
		}
	}
	if bestLen < 0 {
		return selfAccessRule{}, false
	}
	return best, true
}

func selfAccessLevelFor(path string) selfAccessLevel {
	if selfIsConfigRoot(path) {
		return selfAccessReadOnly
	}
	if rule, ok := selfAccessRuleFor(path); ok {
		return rule.level
	}
	// Config is fail-closed: a section or key that is not explicitly allowlisted
	// is blocked, so a new configuration field is denied until it is reviewed.
	if selfIsConfigPath(path) {
		return selfAccessBlocked
	}
	return selfAccessDefault
}

func selfIsConfigRoot(path string) bool {
	segments := selfSignificantSegments(path)
	return len(segments) == 1 && segments[0] == "config"
}

func selfIsConfigPath(path string) bool {
	segments := selfSignificantSegments(path)
	return len(segments) > 0 && segments[0] == "config"
}

// selfPathDenied rejects metaprogramming-style and shadow segments before any
// rule lookup: empty parts, traversal, and underscore-prefixed keys used for
// internal/shadow fields (for example _auth_token).
func selfPathDenied(path string) bool {
	for _, part := range strings.Split(path, ".") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || trimmed == "." || trimmed == ".." ||
			strings.HasPrefix(trimmed, "_") || strings.Contains(trimmed, "__") {
			return true
		}
	}
	return false
}

// selfSensitiveParts are substrings that mark a value as credential-like at any
// depth. They are matched against normalized key names (lowercased, "-" and
// spaces folded to "_").
var selfSensitiveParts = []string{
	"api_key", "secret", "password", "passphrase", "token", "credential", "private_key",
	"access_token", "refresh_token", "auth", "bearer",
}

// selfNonSensitiveKeys lists keys whose names contain a sensitive substring but
// do not hold a credential, so the heuristic does not hide harmless settings.
var selfNonSensitiveKeys = map[string]bool{
	"auth_method":             true,
	"allow_token_query":       true,
	"context_window_tokens":   true,
	"default_token_budget":    true,
	"max_tokens":              true,
	"max_tokens_field":        true,
	"max_total_tokens":        true,
	"used_tokens":             true,
	"total_tokens":            true,
	"output_reserve_tokens":   true,
	"available_tokens":        true,
	"oauth":                   true,
	"summarize_token_percent": true,
}

func selfSensitiveName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer("-", "_", " ", "_").Replace(name)
	if selfNonSensitiveKeys[name] {
		return false
	}
	for _, part := range selfSensitiveParts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

func selfValidationError(key string, rule selfAccessRule) error {
	return fmt.Errorf("%s must be between %d and %d", key, rule.min, rule.max)
}

// selfRuntimeRoots are the writable keys whose sub-paths must be rejected
// rather than treated as scratchpad notes.
var selfRuntimeRoots = map[string]bool{
	"max_iterations":        true,
	"context_window_tokens": true,
	"model":                 true,
	"model_preset":          true,
}

// validateSelfPath checks a read/write target. Blank, traversal, shadow
// (underscore-prefixed) and BLOCKED paths are denied, as are paths containing a
// credential-like segment. context_window_tokens is exempt because it is in
// selfNonSensitiveKeys.
func validateSelfPath(path string) error {
	if selfPathDenied(path) || selfAccessLevelFor(path) == selfAccessBlocked {
		return fmt.Errorf("access denied")
	}
	for _, part := range strings.Split(path, ".") {
		if selfSensitiveName(part) {
			return fmt.Errorf("access denied")
		}
	}
	return nil
}

func isSelfReadOnly(path string) bool {
	switch selfAccessLevelFor(path) {
	case selfAccessBlocked, selfAccessReadOnly:
		return true
	default:
		return false
	}
}

// isSelfWritableRuntimeKey reports whether path is exactly a RESTRICTED runtime
// key. Sub-paths (for example max_iterations.x) are not writable.
func isSelfWritableRuntimeKey(path string) bool {
	return selfAccessLevelFor(path) == selfAccessRestricted && !strings.Contains(path, ".")
}

func isSelfRuntimeRoot(path string) bool {
	return selfRuntimeRoots[strings.Split(path, ".")[0]]
}

// selfAccessLevelName renders an access level for the self tool's list,
// schema and explain output.
func selfAccessLevelName(level selfAccessLevel) string {
	switch level {
	case selfAccessBlocked:
		return "BLOCKED"
	case selfAccessReadOnly:
		return "READ_ONLY"
	case selfAccessRestricted:
		return "RESTRICTED"
	default:
		return "DEFAULT"
	}
}

// selfRangeLabel describes the numeric range a RESTRICTED rule accepts. It is
// empty for rules that take no numeric range (for example model_preset).
func selfRangeLabel(rule selfAccessRule) string {
	if rule.level != selfAccessRestricted || (rule.min == 0 && rule.max == 0) {
		return ""
	}
	return fmt.Sprintf("%d-%d", rule.min, rule.max)
}

// selfRedactedValue marks a configuration value that the self tool hides.
const selfRedactedValue = "[REDACTED]"

// redactedConfigSnapshot serializes the full configuration to a JSON-safe map
// with credentials removed. SecureString/SecureStrings fields are dropped by
// their own JSON marshaling; remaining plaintext secrets are replaced using the
// sensitive-name heuristic, and sections at the BLOCKED access level are
// omitted entirely.
func redactedConfigSnapshot(cfg *config.Config) map[string]any {
	if cfg == nil {
		return nil
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return map[string]any{"error": "configuration is not serializable"}
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return map[string]any{"error": "configuration is not serializable"}
	}
	redacted, _ := redactSelfConfigValue("config", raw).(map[string]any)
	return redacted
}

// redactSelfConfigValue walks a decoded config, applying the access table and
// the sensitive-name heuristic. path is the snapshot path of value.
func redactSelfConfigValue(path string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			child := key
			if path != "" {
				child = path + "." + key
			}
			if selfAccessLevelFor(child) == selfAccessBlocked {
				continue
			}
			if selfSensitiveName(key) {
				out[key] = selfRedactedValue
				continue
			}
			out[key] = redactSelfConfigValue(child, item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactSelfConfigValue(path, item)
		}
		return out
	default:
		return value
	}
}

func resolveSelfPath(view map[string]any, path string) (any, error) {
	var current any = view
	for _, part := range strings.Split(path, ".") {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("key not found: %s", path)
		}
		value, ok := mapping[part]
		if !ok {
			return nil, fmt.Errorf("key not found: %s", path)
		}
		current = value
	}
	return current, nil
}
