package agent

import (
	"fmt"
	"strconv"
	"strings"
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
	{path: "web_config", level: selfAccessReadOnly},
	{path: "exec_config", level: selfAccessReadOnly},
	{path: "model_presets", level: selfAccessReadOnly},
	{path: "config", level: selfAccessReadOnly},

	// Config sections. BLOCKED first by longest match.
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
	if rule, ok := selfAccessRuleFor(path); ok {
		return rule.level
	}
	return selfAccessDefault
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
