package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	maxSelfRuntimeKeys = 64
	maxSelfNoteKeyLen  = 128
	maxSelfNoteBytes   = 1024
)

// RuntimeControl is the read/write boundary exposed to SelfTool.
type RuntimeControl interface {
	Snapshot(ctx context.Context) map[string]any
	Set(ctx context.Context, key string, value any) error
}

type selfSession struct{ agent, session string }

// SelfTool gives the agent a filtered view of its runtime and a session-local
// scratchpad. ToolRegistry.Clone shares tool instances, so state remains keyed
// by agent and session instead of being copied into subagent registries.
type SelfTool struct {
	runtime    RuntimeControl
	allowSet   bool
	mu         sync.Mutex
	scratchpad map[selfSession]map[string]any
}

func NewSelfTool(runtime RuntimeControl, allowSet bool) *SelfTool {
	return &SelfTool{
		runtime:    runtime,
		allowSet:   allowSet,
		scratchpad: make(map[selfSession]map[string]any),
	}
}

func (t *SelfTool) Name() string { return "self" }

func (t *SelfTool) Description() string {
	base := "Inspect PicoClaw's filtered runtime state or keep temporary notes in this session's scratchpad.\n" +
		"Actions: check, set.\n" +
		"- check (no key): show the runtime snapshot and scratchpad.\n" +
		"- check (key): inspect a value; dot paths are supported (for example, " +
		"web_config.enabled or request.channel). Available fields include " +
		"max_iterations, context_window_tokens, model, model_preset, model_presets, " +
		"workspace, tool_names, config, web_config, exec_config, and request.channel/chat_id/sender_id.\n" +
		"config holds the full PicoClaw configuration with credentials removed " +
		"(secure fields are dropped, secret-like keys are shown as [REDACTED], and " +
		"infrastructure/execution sections are omitted).\n" +
		"- set (key, value): change max_iterations (integer 1–100) or " +
		"context_window_tokens (integer 4096–1000000) for this session, " +
		"effective immediately, select a configured " +
		"model_preset for this session (effective next turn), or store a JSON-safe " +
		"scratchpad note (null deletes it). Scratchpad notes are shared across turns in this " +
		"session, cleared by /clear, lost on restart, and limited to 64 keys, 128-byte keys, " +
		"1024 bytes of JSON per value, and 10 levels of nesting.\n" +
		"Direct model changes are disabled. The runtime snapshot " +
		"fields workspace, config, web_config, exec_config, model_presets, tool_names, and request " +
		"are read-only.\n" +
		"Use check for model/settings questions or to diagnose tool behavior; check " +
		"context_window_tokens and max_iterations before a large task. Use model_preset " +
		"when asked to switch to a configured model, and scratchpad notes for " +
		"session-only reminders."
	if t.allowSet {
		return base + "\nIMPORTANT: Before using set, predict its impact. Warn the user before " +
			"critical changes, such as switching the session's model preset."
	}
	return base + "\nREAD-ONLY MODE: set is disabled."
}

func (t *SelfTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"check", "set"},
				"description": "Use check to inspect runtime state or set to update an allowed setting or scratchpad note.",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "Optional target. For check, omit it for a full snapshot or use a dot path such as web_config.enabled or request.channel. For set, use max_iterations, model_preset, or a scratchpad note key.",
			},
			"value": map[string]any{
				"description": "Required for set. Use an integer from 1 to 100 for max_iterations, a configured preset name for model_preset, or a JSON-safe value for a scratchpad note (maximum 64 keys, 1024 bytes per value, and 10 nesting levels; null deletes the note).",
			},
		},
		"required": []string{"action"},
	}
}

func (t *SelfTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	sessionID := selfSessionID(ctx)
	log := func(detail string) {
		logger.InfoCF("agent", "SelfTool audit", map[string]any{
			"event":   fmt.Sprintf("self.%s | %s | session:%s", action, detail, sessionID),
			"agent":   tools.ToolAgentID(ctx),
			"session": sessionID,
		})
	}
	key, _ := args["key"].(string)
	if rawKey, exists := args["key"]; exists && rawKey != nil {
		if _, ok := rawKey.(string); !ok {
			log("REJECTED invalid key type")
			return tools.ErrorResult("key must be a string")
		}
	}
	key = strings.TrimSpace(key)
	for name := range args {
		if name != "action" && name != "key" && name != "value" {
			log("REJECTED unknown argument")
			return tools.ErrorResult("unknown argument: " + name)
		}
	}
	if action != "check" && action != "set" {
		log("REJECTED invalid action")
		return tools.ErrorResult("action must be check or set")
	}
	if t.runtime == nil {
		log("REJECTED unavailable runtime")
		return tools.ErrorResult("runtime control is unavailable")
	}
	if action == "check" {
		if _, exists := args["value"]; exists {
			log("REJECTED unexpected value")
			return tools.ErrorResult("value is only accepted for set")
		}
		view := t.runtime.Snapshot(ctx)
		view["scratchpad"] = t.sessionNotes(ctx)
		if key == "" {
			log("check summary")
			encoded, err := json.Marshal(view)
			if err != nil {
				return tools.ErrorResult("runtime snapshot is not JSON-compatible")
			}
			return tools.NewToolResult(string(encoded))
		}
		if err := validateSelfPath(key); err != nil {
			log("BLOCKED " + key)
			return tools.ErrorResult(err.Error())
		}
		value, err := resolveSelfPath(view, key)
		if err != nil {
			if note, ok := t.getNote(ctx, key); ok {
				log("check " + key)
				return tools.NewToolResult(formatSelfValue(note))
			}
			log("REJECTED " + key)
			return tools.ErrorResult(err.Error())
		}
		log("check " + key)
		return tools.NewToolResult(formatSelfValue(value))
	}
	if !t.allowSet {
		log("READ_ONLY " + key)
		return tools.ErrorResult("set is disabled: self tool is read-only")
	}
	if key == "" {
		log("REJECTED empty key")
		return tools.ErrorResult("set requires a non-empty key")
	}
	value, exists := args["value"]
	if !exists {
		log("REJECTED missing value")
		return tools.ErrorResult("set requires value")
	}
	if err := validateSelfPath(key); err != nil {
		log("BLOCKED " + key)
		return tools.ErrorResult(err.Error())
	}
	if key == "model" {
		log("READ_ONLY " + key)
		return tools.ErrorResult("model cannot be changed directly through SelfTool; use model_preset")
	}
	if isSelfReadOnly(key) {
		log("READ_ONLY " + key)
		return tools.ErrorResult("property is read-only: " + key)
	}
	if isSelfWritableRuntimeKey(key) {
		if err := t.runtime.Set(ctx, key, value); err != nil {
			log("REJECTED " + key)
			return tools.ErrorResult(err.Error())
		}
		log("set " + key)
		return tools.NewToolResult(selfSetMessage(key, value))
	}
	if isSelfRuntimeRoot(key) {
		log("REJECTED invalid runtime path " + key)
		return tools.ErrorResult("key is not a writable runtime property: " + key)
	}
	if err := validateSelfJSON(value, 0); err != nil {
		log("REJECTED JSON value")
		return tools.ErrorResult(err.Error())
	}
	if err := validateSelfSensitiveKeys(value); err != nil {
		log("BLOCKED sensitive scratchpad key")
		return tools.ErrorResult(err.Error())
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		log("REJECTED JSON value")
		return tools.ErrorResult("value must be JSON-compatible")
	}
	if len(key) > maxSelfNoteKeyLen || len(encoded) > maxSelfNoteBytes {
		log("REJECTED oversized note")
		return tools.ErrorResult(fmt.Sprintf(
			"scratchpad keys are limited to %d bytes and values to %d bytes of JSON",
			maxSelfNoteKeyLen, maxSelfNoteBytes,
		))
	}
	var clone any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		log("REJECTED JSON value")
		return tools.ErrorResult("value must be JSON-compatible")
	}
	if err := t.setNote(ctx, key, clone); err != nil {
		log("REJECTED " + key)
		return tools.ErrorResult(err.Error())
	}
	log("set scratchpad " + key)
	return tools.NewToolResult("scratchpad updated: " + key)
}

func (t *SelfTool) sessionNotes(ctx context.Context) map[string]any {
	key := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneSelfMap(t.scratchpad[key])
}

func (t *SelfTool) getNote(ctx context.Context, key string) (any, bool) {
	session := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.scratchpad[session][key]
	return value, ok
}

func (t *SelfTool) setNote(ctx context.Context, key string, value any) error {
	session := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.scratchpad[session]
	if value == nil {
		delete(entries, key)
		return nil
	}
	if entries == nil {
		entries = make(map[string]any)
		t.scratchpad[session] = entries
	}
	if _, exists := entries[key]; !exists && len(entries) >= maxSelfRuntimeKeys {
		return fmt.Errorf(
			"scratchpad is full (%d keys); remove an unused key first",
			maxSelfRuntimeKeys,
		)
	}
	entries[key] = value
	return nil
}

// ClearSession discards a session's scratchpad when its conversation is cleared.
func (t *SelfTool) ClearSession(agentID, sessionKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.scratchpad, selfSession{agent: agentID, session: sessionKey})
}

// clearSessionRuntime drops what the self tool and model_preset kept for a
// session, so /clear starts it again from the configured agent settings.
func (al *AgentLoop) clearSessionRuntime(agent *AgentInstance, sessionKey string) {
	key := sessionModelKey{agentID: agent.ID, sessionKey: sessionKey}
	al.clearSessionModelOverride(key)
	al.sessionRuntime.Delete(key)
	if tool, ok := agent.Tools.Get("self"); ok {
		if self, ok := tool.(*SelfTool); ok {
			self.ClearSession(agent.ID, sessionKey)
		}
	}
}

func selfSessionFor(ctx context.Context) selfSession {
	agentID, sessionID := tools.ToolAgentID(ctx), tools.ToolSessionKey(ctx)
	if ts := turnStateFromContext(ctx); ts != nil {
		if agentID == "" {
			agentID = ts.agentID
		}
		if sessionID == "" {
			sessionID = ts.sessionKey
		}
	}
	if sessionID == "" && (tools.ToolChannel(ctx) != "" || tools.ToolChatID(ctx) != "") {
		sessionID = tools.ToolChannel(ctx) + ":" + tools.ToolChatID(ctx)
	}
	if agentID == "" {
		agentID = "unknown"
	}
	if sessionID == "" {
		sessionID = "unknown"
	}
	return selfSession{agent: agentID, session: sessionID}
}

func selfSessionID(ctx context.Context) string {
	return selfSessionFor(ctx).session
}

func cloneSelfMap(value map[string]any) map[string]any {
	if len(value) == 0 {
		return map[string]any{}
	}
	clone := make(map[string]any, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}
