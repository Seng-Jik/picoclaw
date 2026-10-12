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
	maxSelfHistory     = 20
)

// selfReadOnlyActions never change runtime state and are allowed even when the
// tool is configured without allow_set.
var selfReadOnlyActions = map[string]bool{
	"check": true, "list": true, "schema": true, "explain": true,
	"diff": true, "history": true, "logs": true,
}

// selfWriteActions change session state and require allow_set.
var selfWriteActions = map[string]bool{
	"set": true, "reset": true, "undo": true,
}

// selfKnownActions is the union of the read-only and write action sets.
var selfKnownActions = func() map[string]bool {
	actions := make(map[string]bool, len(selfReadOnlyActions)+len(selfWriteActions))
	for name := range selfReadOnlyActions {
		actions[name] = true
	}
	for name := range selfWriteActions {
		actions[name] = true
	}
	return actions
}()

// RuntimeControl is the read/write boundary exposed to SelfTool.
type RuntimeControl interface {
	Snapshot(ctx context.Context) map[string]any
	Set(ctx context.Context, key string, value any) error
	// Reset clears a session-scoped override so the configured agent value
	// applies again. It is the inverse of Set for key.
	Reset(ctx context.Context, key string) error
}

type selfSession struct{ agent, session string }

// selfChange records one successful session write so history and undo can
// replay or revert it. before is the effective value at write time and
// beforeSet is false when the scratchpad key did not exist.
type selfChange struct {
	action    string
	key       string
	before    any
	beforeSet bool
	after     any
}

// SelfTool gives the agent a filtered view of its runtime and a session-local
// scratchpad. ToolRegistry.Clone shares tool instances, so state remains keyed
// by agent and session instead of being copied into subagent registries.
type SelfTool struct {
	runtime    RuntimeControl
	allowSet   bool
	mu         sync.Mutex
	scratchpad map[selfSession]map[string]any
	history    map[selfSession][]selfChange
}

func NewSelfTool(runtime RuntimeControl, allowSet bool) *SelfTool {
	return &SelfTool{
		runtime:    runtime,
		allowSet:   allowSet,
		scratchpad: make(map[selfSession]map[string]any),
		history:    make(map[selfSession][]selfChange),
	}
}

func (t *SelfTool) Name() string { return "self" }

func (t *SelfTool) Description() string {
	base := "Inspect PicoClaw's filtered runtime state, learn the self-tool schema, " +
		"or keep temporary notes in this session's scratchpad.\n" +
		"Actions: check, list, schema, explain, diff, history, logs, set, reset, undo.\n" +
		"- check (no key): show the runtime snapshot and scratchpad.\n" +
		"- check (key): inspect a value; dot paths are supported (for example, " +
		"web_config.enabled or request.channel). Available fields include " +
		"max_iterations, context_window_tokens, model, model_preset, model_presets, " +
		"session_overrides, defaults, workspace, tool_names, config, web_config, " +
		"exec_config, and request.channel/chat_id/sender_id.\n" +
		"config holds the full PicoClaw configuration with credentials removed " +
		"(secure fields are dropped, secret-like keys are shown as [REDACTED], and " +
		"infrastructure/execution sections are omitted).\n" +
		"- list (key): enumerate the children of a snapshot node with their access " +
		"level and numeric range; omit key to list the snapshot root.\n" +
		"- schema: print every access rule (path, level, range) this tool enforces.\n" +
		"- explain (key): describe one path, its access level, whether it is " +
		"writable, and its current value when readable.\n" +
		"- diff: compare session overrides against the configured agent defaults.\n" +
		"- history: show the recent session writes this tool can undo.\n" +
		"- logs (key): tail PicoClaw's own log files; key filters by level name " +
		"(debug/info/warn/error/fatal) or by substring. Secrets are redacted.\n" +
		"- set (key, value): change max_iterations (integer 1–100) or " +
		"context_window_tokens (integer 4096–1000000) for this session, " +
		"effective immediately, select a configured " +
		"model_preset for this session (effective next turn), or store a JSON-safe " +
		"scratchpad note (null deletes it). Scratchpad notes are shared across turns in this " +
		"session, cleared by /clear, lost on restart, and limited to 64 keys, 128-byte keys, " +
		"1024 bytes of JSON per value, and 10 levels of nesting.\n" +
		"- reset (key): drop one session override (max_iterations, " +
		"context_window_tokens, or model_preset) so the configured value applies again.\n" +
		"- undo: revert the most recent session write.\n" +
		"Direct model changes are disabled. The runtime snapshot " +
		"fields workspace, config, web_config, exec_config, model_presets, tool_names, " +
		"defaults, session_overrides, and request are read-only.\n" +
		"Use check for model/settings questions or to diagnose tool behavior; check " +
		"context_window_tokens and max_iterations before a large task. Use model_preset " +
		"when asked to switch to a configured model, and scratchpad notes for " +
		"session-only reminders."
	if t.allowSet {
		return base + "\nIMPORTANT: Before using set, predict its impact. Warn the user before " +
			"critical changes, such as switching the session's model preset."
	}
	return base + "\nREAD-ONLY MODE: set, reset and undo are disabled."
}

func (t *SelfTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{
					"check", "list", "schema", "explain", "diff", "history", "logs",
					"set", "reset", "undo",
				},
				"description": "check inspects the snapshot; list, schema, explain, diff, " +
					"history and logs are read-only; set, reset and undo change session state.",
			},
			"key": map[string]any{
				"type": "string",
				"description": "Optional target. For check/list/explain use a dot path such as " +
					"web_config.enabled or request.channel; for set, reset use max_iterations, " +
					"context_window_tokens, model_preset or a scratchpad note key; for logs use a " +
					"level name or substring filter.",
			},
			"value": map[string]any{
				"description": "Required for set. Use an integer from 1 to 100 for max_iterations, " +
					"a configured preset name for model_preset, or a JSON-safe value for a " +
					"scratchpad note (maximum 64 keys, 1024 bytes per value, and 10 nesting " +
					"levels; null deletes the note).",
			},
		},
		"required": []string{"action"},
	}
}

func (t *SelfTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if !selfKnownActions[action] {
		t.audit(ctx, action, "REJECTED invalid action")
		return tools.ErrorResult(
			"action must be one of check, list, schema, explain, diff, history, logs, set, reset, undo",
		)
	}
	key, _ := args["key"].(string)
	if rawKey, exists := args["key"]; exists && rawKey != nil {
		if _, ok := rawKey.(string); !ok {
			t.audit(ctx, action, "REJECTED invalid key type")
			return tools.ErrorResult("key must be a string")
		}
	}
	key = strings.TrimSpace(key)
	for name := range args {
		if name != "action" && name != "key" && name != "value" {
			t.audit(ctx, action, "REJECTED unknown argument")
			return tools.ErrorResult("unknown argument: " + name)
		}
	}
	_, hasValue := args["value"]
	if hasValue && action != "set" {
		t.audit(ctx, action, "REJECTED unexpected value")
		return tools.ErrorResult("value is only accepted for set")
	}
	if selfWriteActions[action] && !t.allowSet {
		t.audit(ctx, action, "READ_ONLY")
		return tools.ErrorResult("set is disabled: self tool is read-only")
	}

	switch action {
	case "check":
		return t.executeCheck(ctx, key)
	case "list":
		return t.executeList(ctx, key)
	case "schema":
		return t.executeSchema(ctx)
	case "explain":
		return t.executeExplain(ctx, key)
	case "diff":
		return t.executeDiff(ctx)
	case "history":
		return t.executeHistory(ctx)
	case "logs":
		return t.executeLogs(ctx, key)
	case "set":
		return t.executeSet(ctx, key, args)
	case "reset":
		return t.executeReset(ctx, key)
	case "undo":
		return t.executeUndo(ctx)
	}
	return tools.ErrorResult("action must be one of check, list, schema, explain, diff, history, logs, set, reset, undo")
}

func (t *SelfTool) audit(ctx context.Context, action, detail string) {
	logger.InfoCF("agent", "SelfTool audit", map[string]any{
		"event":   fmt.Sprintf("self.%s | %s | session:%s", action, detail, selfSessionID(ctx)),
		"agent":   tools.ToolAgentID(ctx),
		"session": selfSessionID(ctx),
	})
}

// runtimeView returns the filtered runtime snapshot plus this session's
// scratchpad, or an error result when the runtime is unavailable.
func (t *SelfTool) runtimeView(ctx context.Context) (map[string]any, *tools.ToolResult) {
	if t.runtime == nil {
		return nil, tools.ErrorResult("runtime control is unavailable")
	}
	view := t.runtime.Snapshot(ctx)
	view["scratchpad"] = t.sessionNotes(ctx)
	return view, nil
}

func (t *SelfTool) executeCheck(ctx context.Context, key string) *tools.ToolResult {
	view, errResult := t.runtimeView(ctx)
	if errResult != nil {
		return errResult
	}
	if key == "" {
		t.audit(ctx, "check", "summary")
		encoded, err := json.Marshal(view)
		if err != nil {
			return tools.ErrorResult("runtime snapshot is not JSON-compatible")
		}
		return tools.NewToolResult(string(encoded))
	}
	if err := validateSelfPath(key); err != nil {
		t.audit(ctx, "check", "BLOCKED "+key)
		return tools.ErrorResult(err.Error())
	}
	value, err := resolveSelfPath(view, key)
	if err != nil {
		if note, ok := t.getNote(ctx, key); ok {
			t.audit(ctx, "check", key)
			return tools.NewToolResult(formatSelfValue(note))
		}
		t.audit(ctx, "check", "REJECTED "+key)
		return tools.ErrorResult(err.Error())
	}
	t.audit(ctx, "check", key)
	return tools.NewToolResult(formatSelfValue(value))
}

func (t *SelfTool) executeSet(ctx context.Context, key string, args map[string]any) *tools.ToolResult {
	if key == "" {
		t.audit(ctx, "set", "REJECTED empty key")
		return tools.ErrorResult("set requires a non-empty key")
	}
	value, exists := args["value"]
	if !exists {
		t.audit(ctx, "set", "REJECTED missing value")
		return tools.ErrorResult("set requires value")
	}
	if err := validateSelfPath(key); err != nil {
		t.audit(ctx, "set", "BLOCKED "+key)
		return tools.ErrorResult(err.Error())
	}
	if key == "model" {
		t.audit(ctx, "set", "READ_ONLY "+key)
		return tools.ErrorResult("model cannot be changed directly through SelfTool; use model_preset")
	}
	if isSelfReadOnly(key) {
		t.audit(ctx, "set", "READ_ONLY "+key)
		return tools.ErrorResult("property is read-only: " + key)
	}
	if isSelfWritableRuntimeKey(key) {
		if t.runtime == nil {
			t.audit(ctx, "set", "REJECTED unavailable runtime")
			return tools.ErrorResult("runtime control is unavailable")
		}
		var before any
		if view, errResult := t.runtimeView(ctx); errResult == nil {
			before, _ = resolveSelfPath(view, key)
		}
		if err := t.runtime.Set(ctx, key, value); err != nil {
			t.audit(ctx, "set", "REJECTED "+key)
			return tools.ErrorResult(err.Error())
		}
		t.record(ctx, selfChange{action: "set", key: key, before: before, beforeSet: true, after: value})
		t.audit(ctx, "set", key)
		return tools.NewToolResult(selfSetMessage(key, value))
	}
	if isSelfRuntimeRoot(key) {
		t.audit(ctx, "set", "REJECTED invalid runtime path "+key)
		return tools.ErrorResult("key is not a writable runtime property: " + key)
	}
	if err := validateSelfJSON(value, 0); err != nil {
		t.audit(ctx, "set", "REJECTED JSON value")
		return tools.ErrorResult(err.Error())
	}
	if err := validateSelfSensitiveKeys(value); err != nil {
		t.audit(ctx, "set", "BLOCKED sensitive scratchpad key")
		return tools.ErrorResult(err.Error())
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.audit(ctx, "set", "REJECTED JSON value")
		return tools.ErrorResult("value must be JSON-compatible")
	}
	if len(key) > maxSelfNoteKeyLen || len(encoded) > maxSelfNoteBytes {
		t.audit(ctx, "set", "REJECTED oversized note")
		return tools.ErrorResult(fmt.Sprintf(
			"scratchpad keys are limited to %d bytes and values to %d bytes of JSON",
			maxSelfNoteKeyLen, maxSelfNoteBytes,
		))
	}
	var clone any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.audit(ctx, "set", "REJECTED JSON value")
		return tools.ErrorResult("value must be JSON-compatible")
	}
	before, beforeSet := t.getNote(ctx, key)
	if err := t.setNote(ctx, key, clone); err != nil {
		t.audit(ctx, "set", "REJECTED "+key)
		return tools.ErrorResult(err.Error())
	}
	t.record(ctx, selfChange{action: "set", key: key, before: before, beforeSet: beforeSet, after: clone})
	t.audit(ctx, "set", "scratchpad "+key)
	return tools.NewToolResult("scratchpad updated: " + key)
}

func (t *SelfTool) executeReset(ctx context.Context, key string) *tools.ToolResult {
	if key == "" {
		t.audit(ctx, "reset", "REJECTED empty key")
		return tools.ErrorResult("reset requires a non-empty key")
	}
	if !isSelfRuntimeRoot(key) || key == "model" {
		t.audit(ctx, "reset", "REJECTED "+key)
		return tools.ErrorResult("reset supports max_iterations, context_window_tokens and model_preset")
	}
	if t.runtime == nil {
		t.audit(ctx, "reset", "REJECTED unavailable runtime")
		return tools.ErrorResult("runtime control is unavailable")
	}
	var before any
	if view, errResult := t.runtimeView(ctx); errResult == nil {
		before, _ = resolveSelfPath(view, key)
	}
	if err := t.runtime.Reset(ctx, key); err != nil {
		t.audit(ctx, "reset", "REJECTED "+key)
		return tools.ErrorResult(err.Error())
	}
	t.record(ctx, selfChange{action: "reset", key: key, before: before, beforeSet: true})
	t.audit(ctx, "reset", key)
	return tools.NewToolResult(key + " reset to the configured default for this session")
}

func (t *SelfTool) executeUndo(ctx context.Context) *tools.ToolResult {
	entries := t.sessionHistory(ctx)
	if len(entries) == 0 {
		t.audit(ctx, "undo", "REJECTED nothing to undo")
		return tools.ErrorResult("no session changes to undo")
	}
	last := entries[len(entries)-1]
	if isSelfRuntimeRoot(last.key) && last.key != "model" {
		if t.runtime == nil {
			t.audit(ctx, "undo", "REJECTED unavailable runtime")
			return tools.ErrorResult("runtime control is unavailable")
		}
		if err := t.runtime.Set(ctx, last.key, last.before); err != nil {
			t.audit(ctx, "undo", "REJECTED "+last.key)
			return tools.ErrorResult(err.Error())
		}
	} else {
		var restore any
		if last.beforeSet {
			restore = last.before
		}
		if err := t.setNote(ctx, last.key, restore); err != nil {
			t.audit(ctx, "undo", "REJECTED "+last.key)
			return tools.ErrorResult(err.Error())
		}
	}
	t.popHistory(ctx)
	t.audit(ctx, "undo", last.action+" "+last.key)
	return tools.NewToolResult("reverted " + last.action + " on " + last.key)
}

func (t *SelfTool) executeHistory(ctx context.Context) *tools.ToolResult {
	entries := t.sessionHistory(ctx)
	if len(entries) == 0 {
		t.audit(ctx, "history", "empty")
		return tools.NewToolResult("no changes recorded in this session")
	}
	records := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		record := map[string]any{"action": entry.action, "key": entry.key}
		if entry.beforeSet {
			record["before"] = entry.before
		}
		if entry.action != "reset" {
			record["after"] = entry.after
		}
		records = append(records, record)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		return tools.ErrorResult("history is not JSON-compatible")
	}
	t.audit(ctx, "history", fmt.Sprintf("%d entries", len(records)))
	return tools.NewToolResult(string(encoded))
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

func (t *SelfTool) sessionHistory(ctx context.Context) []selfChange {
	session := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.history[session]
	clone := make([]selfChange, len(entries))
	copy(clone, entries)
	return clone
}

func (t *SelfTool) record(ctx context.Context, change selfChange) {
	session := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := append(t.history[session], change)
	if len(entries) > maxSelfHistory {
		entries = entries[len(entries)-maxSelfHistory:]
	}
	t.history[session] = entries
}

func (t *SelfTool) popHistory(ctx context.Context) {
	session := selfSessionFor(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.history[session]
	if len(entries) == 0 {
		return
	}
	entries = entries[:len(entries)-1]
	if len(entries) == 0 {
		delete(t.history, session)
		return
	}
	t.history[session] = entries
}

// ClearSession discards a session's scratchpad when its conversation is cleared.
func (t *SelfTool) ClearSession(agentID, sessionKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	session := selfSession{agent: agentID, session: sessionKey}
	delete(t.scratchpad, session)
	delete(t.history, session)
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
