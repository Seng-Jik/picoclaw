package agent

import (
	"encoding/json"

	"github.com/sipeed/picoclaw/pkg/config"
)

// selfRedactedValue marks a configuration value that the self tool hides.
const selfRedactedValue = "[REDACTED]"

// redactedConfigSnapshot serializes the full configuration to a JSON-safe map
// with credentials removed. SecureString/SecureStrings fields are dropped by
// their own JSON marshaling; remaining plaintext secrets (for example
// `elevenlabs_api_key`, MCP `env` entries, or `Authorization` headers) are
// replaced by name using the same heuristic as selfSensitiveName.
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
	redacted, _ := redactSelfConfigValue(raw).(map[string]any)
	return redacted
}

func redactSelfConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if selfSensitiveName(key) {
				out[key] = selfRedactedValue
				continue
			}
			out[key] = redactSelfConfigValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactSelfConfigValue(item)
		}
		return out
	default:
		return value
	}
}
