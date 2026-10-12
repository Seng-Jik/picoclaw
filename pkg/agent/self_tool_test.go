package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type selfToolTestRuntime struct {
	view     map[string]any
	setKey   string
	setValue any
	resetKey string
}

func (r *selfToolTestRuntime) Snapshot(context.Context) map[string]any { return r.view }
func (r *selfToolTestRuntime) Set(_ context.Context, key string, value any) error {
	r.setKey, r.setValue = key, value
	return nil
}
func (r *selfToolTestRuntime) Reset(_ context.Context, key string) error {
	r.resetKey = key
	return nil
}

func runSelfTool(tool *SelfTool, ctx context.Context, action, key string, value any, hasValue bool) *tools.ToolResult {
	args := map[string]any{"action": action}
	if key != "" {
		args["key"] = key
	}
	if hasValue {
		args["value"] = value
	}
	return tool.Execute(ctx, args)
}

func selfTestContext(session string) context.Context {
	ctx := tools.WithToolContext(context.Background(), "telegram", "chat-1")
	return tools.WithToolSessionContext(ctx, "main", session, nil)
}

func TestSelfToolDescriptionAndParameters(t *testing.T) {
	readOnly := NewSelfTool(nil, false)
	for _, want := range []string{
		"web_config.enabled", "request.channel", "max_iterations (integer 1–100)",
		"64 keys", "10 levels", "READ-ONLY MODE: set, reset and undo are disabled.",
	} {
		if !strings.Contains(readOnly.Description(), want) {
			t.Errorf("read-only description missing %q", want)
		}
	}

	readWrite := NewSelfTool(nil, true)
	if !strings.Contains(readWrite.Description(), "Warn the user before critical changes") {
		t.Error("read-write description does not warn before critical changes")
	}
	parameters := readWrite.Parameters()["properties"].(map[string]any)
	action := parameters["action"].(map[string]any)
	if got, ok := action["enum"].([]string); !ok || len(got) != 10 || got[0] != "check" || got[1] != "list" ||
		got[9] != "undo" {
		t.Errorf("action enum = %#v, want ten actions starting check/list and ending undo", action["enum"])
	}
	for _, name := range []string{"action", "key", "value"} {
		if _, ok := parameters[name].(map[string]any)["description"].(string); !ok {
			t.Errorf("parameter %q has no description", name)
		}
	}
}

func TestSelfToolCheckFiltersAndResolvesPaths(t *testing.T) {
	runtime := &selfToolTestRuntime{view: map[string]any{
		"max_iterations": 20,
		"web_config":     map[string]any{"format": "markdown", "provider": "duckduckgo"},
		"request":        map[string]any{"channel": "telegram"},
	}}
	tool := NewSelfTool(runtime, false)
	ctx := selfTestContext("session-1")

	if got := runSelfTool(tool, ctx, "check", "web_config.format", nil, false).ForLLM; got != "markdown" {
		t.Fatalf("dot-path result = %q, want markdown", got)
	}
	if got := runSelfTool(tool, ctx, "check", "web_config.provider", nil, false).ForLLM; got != "duckduckgo" {
		t.Fatalf("web_config.provider = %q, want duckduckgo", got)
	}
	if got := runSelfTool(tool, ctx, "check", "request.channel", nil, false).ForLLM; got != "telegram" {
		t.Fatalf("request channel = %q, want telegram", got)
	}
	for _, key := range []string{"provider", "request.api_key"} {
		if result := runSelfTool(tool, ctx, "check", key, nil, false); !result.IsError {
			t.Errorf("check %q returned a value", key)
		}
	}
	for _, action := range []string{"inspect", "modify"} {
		if result := runSelfTool(tool, ctx, action, "request.channel", nil, false); !result.IsError {
			t.Errorf("undocumented action %q was accepted", action)
		}
	}
	if result := runSelfTool(tool, ctx, "set", "note", "value", true); !result.IsError {
		t.Fatal("set succeeded in read-only mode")
	}
}

func TestSelfToolCheckWithoutKeyReturnsSnapshotValues(t *testing.T) {
	runtime := &selfToolTestRuntime{view: map[string]any{
		"max_iterations":        20,
		"context_window_tokens": 32768,
		"model":                 "local",
		"model_preset":          "local",
		"workspace":             "/tmp/ws",
		"tool_names":            []string{"self", "read_file"},
		"web_config":            map[string]any{"format": "markdown"},
		"request":               map[string]any{"channel": "telegram"},
	}}
	tool := NewSelfTool(runtime, true)
	ctx := selfTestContext("session-1")
	if result := runSelfTool(tool, ctx, "set", "plan", "ship it", true); result.IsError {
		t.Fatalf("set scratchpad: %s", result.ForLLM)
	}

	got := runSelfTool(tool, ctx, "check", "", nil, false).ForLLM
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(got), &snapshot); err != nil {
		t.Fatalf("snapshot is not JSON: %v\n%s", err, got)
	}
	if snapshot["max_iterations"] != float64(20) || snapshot["model"] != "local" ||
		snapshot["context_window_tokens"] != float64(32768) {
		t.Errorf("snapshot is missing runtime values: %s", got)
	}
	if notes, _ := snapshot["scratchpad"].(map[string]any); notes["plan"] != "ship it" {
		t.Errorf("snapshot scratchpad = %v, want the plan note", snapshot["scratchpad"])
	}
}

func TestSelfToolScratchpadIsSessionScopedAndJSONSafe(t *testing.T) {
	tool := NewSelfTool(&selfToolTestRuntime{view: map[string]any{}}, true)
	ctx := selfTestContext("session-1")
	value := map[string]any{"done": true}
	if result := runSelfTool(tool, ctx, "set", "plan", value, true); result.IsError {
		t.Fatalf("set scratchpad: %s", result.ForLLM)
	}
	if got := runSelfTool(tool, ctx, "check", "plan", nil, false).ForLLM; got != `{"done":true}` {
		t.Fatalf("scratchpad value = %q", got)
	}
	if result := runSelfTool(tool, selfTestContext("session-2"), "check", "plan", nil, false); !result.IsError {
		t.Fatal("scratchpad value leaked across sessions")
	}
	if result := runSelfTool(tool, ctx, "set", "unsafe", map[string]any{"api_key": "secret"}, true); !result.IsError {
		t.Fatal("scratchpad accepted a sensitive key")
	}
	if result := runSelfTool(tool, ctx, "set", "deep", nestedSelfValue(11), true); !result.IsError {
		t.Fatal("scratchpad accepted nesting deeper than 10")
	}
	if result := runSelfTool(tool, ctx, "set", "request.channel", "cli", true); !result.IsError {
		t.Fatal("set changed a read-only request field")
	}
}

func TestSelfToolMaxIterationsIsSessionScoped(t *testing.T) {
	agent := &AgentInstance{
		ID: "main", modelMu: &sync.RWMutex{}, Tools: tools.NewToolRegistry(), MaxIterations: 20,
	}
	al := &AgentLoop{}
	tool := NewSelfTool(&agentRuntimeControl{agent: agent}, true)
	turnContext := func(session string) (context.Context, *turnState) {
		ts := newTurnState(
			agent,
			processOptions{Dispatch: DispatchRequest{SessionKey: session}},
			turnEventScope{agentID: agent.ID, sessionKey: session, turnID: "turn-" + session},
		)
		return WithAgentLoop(withTurnState(selfTestContext(session), ts), al), ts
	}
	ctx, ts := turnContext("session-1")
	otherCtx, otherTS := turnContext("session-2")

	if result := runSelfTool(tool, ctx, "set", "max_iterations", float64(35), true); result.IsError {
		t.Fatalf("set max_iterations: %s", result.ForLLM)
	}
	if got := ts.maxIterations(); got != 35 {
		t.Fatalf("running turn max_iterations = %d, want 35", got)
	}
	if got := agent.MaxIterations; got != 20 {
		t.Fatalf("agent max_iterations = %d, want it to stay 20", got)
	}
	if got := al.sessionIterationLimit("main", "session-1"); got != 35 {
		t.Fatalf("session-1 limit = %d, want 35", got)
	}
	if got := al.sessionIterationLimit("main", "session-2"); got != 0 {
		t.Fatalf("session-2 limit = %d, want none", got)
	}
	if got := otherTS.maxIterations(); got != 20 {
		t.Fatalf("session-2 turn max_iterations = %d, want 20", got)
	}
	if got := runSelfTool(tool, ctx, "check", "max_iterations", nil, false).ForLLM; got != "35" {
		t.Fatalf("session-1 check = %q, want 35", got)
	}
	if got := runSelfTool(tool, otherCtx, "check", "max_iterations", nil, false).ForLLM; got != "20" {
		t.Fatalf("session-2 check = %q, want 20", got)
	}

	for _, value := range []any{true, float64(100.5), float64(0), float64(101)} {
		if result := runSelfTool(tool, ctx, "set", "max_iterations", value, true); !result.IsError {
			t.Errorf("max_iterations accepted invalid value %#v", value)
		}
	}
	result := runSelfTool(tool, selfTestContext("session-3"), "set", "max_iterations", float64(5), true)
	if !result.IsError {
		t.Error("max_iterations changed outside an active agent turn")
	}
}

func TestSelfToolMaxIterationsRequiresSessionKey(t *testing.T) {
	agent := &AgentInstance{
		ID: "main", modelMu: &sync.RWMutex{}, Tools: tools.NewToolRegistry(), MaxIterations: 20,
	}
	al := &AgentLoop{}
	tool := NewSelfTool(&agentRuntimeControl{agent: agent}, true)
	ts := newTurnState(
		agent,
		processOptions{Dispatch: DispatchRequest{SessionKey: ""}},
		turnEventScope{agentID: agent.ID, sessionKey: "", turnID: "turn-keyless"},
	)
	ctx := WithAgentLoop(withTurnState(selfTestContext(""), ts), al)

	if result := runSelfTool(tool, ctx, "set", "max_iterations", float64(35), true); !result.IsError {
		t.Fatal("max_iterations was accepted for a turn without a session key")
	}
	if got := ts.maxIterations(); got != 20 {
		t.Fatalf("keyless turn max_iterations = %d, want 20", got)
	}
	if got := al.sessionIterationLimit("main", ""); got != 0 {
		t.Fatalf("keyless limit = %d, want none; it would leak to other keyless turns", got)
	}
	if got := al.sessionIterationLimit("main", "session-1"); got != 0 {
		t.Fatalf("session-1 limit = %d, want none", got)
	}
}

func TestSelfToolContextWindowTokensIsSessionScoped(t *testing.T) {
	f := newSessionModelFixture(t)
	ctx := f.turnContext("session-a")
	base := f.baseAgent.ContextWindow

	if result := runSelfTool(f.selfTool, ctx, "check", "context_window_tokens", nil, false); result.IsError ||
		result.ForLLM != fmt.Sprint(base) {
		t.Fatalf("check context_window_tokens = %q (error=%v), want %d", result.ForLLM, result.IsError, base)
	}
	for _, value := range []any{float64(100), float64(2000000), float64(0), "big", true} {
		if result := runSelfTool(f.selfTool, ctx, "set", "context_window_tokens", value, true); !result.IsError {
			t.Errorf("context_window_tokens accepted invalid value %#v", value)
		}
	}
	if f.baseAgent.ContextWindow != base {
		t.Fatalf("invalid set changed the agent context window to %d", f.baseAgent.ContextWindow)
	}

	if result := runSelfTool(f.selfTool, ctx, "set", "context_window_tokens", float64(65536), true); result.IsError {
		t.Fatalf("set context_window_tokens: %s", result.ForLLM)
	}
	if f.baseAgent.ContextWindow != base {
		t.Fatalf("agent context window changed to %d, want it to stay %d", f.baseAgent.ContextWindow, base)
	}
	if got := f.al.sessionContextWindow(f.baseAgent.ID, "session-a"); got != 65536 {
		t.Fatalf("session-a context window = %d, want 65536", got)
	}
	if got := f.al.sessionContextWindow(f.baseAgent.ID, "session-b"); got != 0 {
		t.Fatalf("session-b context window = %d, want none", got)
	}
	if got := runSelfTool(f.selfTool, ctx, "check", "context_window_tokens", nil, false).ForLLM; got != "65536" {
		t.Fatalf("check after set = %q, want 65536", got)
	}
	otherCtx := f.turnContext("session-b")
	if got := runSelfTool(f.selfTool, otherCtx, "check", "context_window_tokens", nil, false).ForLLM; got != fmt.Sprint(
		base,
	) {
		t.Fatalf("session-b check = %q, want %d", got, base)
	}
}

func TestSelfToolContextWindowTokensIsNotSensitive(t *testing.T) {
	runtime := &selfToolTestRuntime{view: map[string]any{"context_window_tokens": 32768}}
	tool := NewSelfTool(runtime, true)
	ctx := selfTestContext("session-1")

	if result := runSelfTool(tool, ctx, "check", "context_window_tokens", nil, false); result.IsError ||
		result.ForLLM != "32768" {
		t.Fatalf("check context_window_tokens = %q (error=%v), want 32768", result.ForLLM, result.IsError)
	}
	if result := runSelfTool(tool, ctx, "set", "context_window_tokens", float64(65536), true); result.IsError ||
		runtime.setKey != "context_window_tokens" {
		t.Fatalf(
			"set context_window_tokens = %q (error=%v, runtime key=%q)",
			result.ForLLM, result.IsError, runtime.setKey,
		)
	}
	for _, key := range []string{"access_token", "refresh_token", "context_window_tokens.api_key"} {
		if result := runSelfTool(tool, ctx, "check", key, nil, false); !result.IsError {
			t.Errorf("check %q was allowed", key)
		}
	}
}

func TestSelfInt(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int
		ok    bool
	}{
		{35, 35, true},
		{float64(35), 35, true},
		{json.Number("35"), 35, true},
		{float64(100.5), 0, false},
		{json.Number("35.5"), 0, false},
		{float64(1e30), 0, false},
		{"35", 0, false},
		{true, 0, false},
		{nil, 0, false},
	} {
		got, err := selfInt(tc.value)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("selfInt(%#v) = %d, %v; want %d, ok=%v", tc.value, got, err, tc.want, tc.ok)
		}
	}
}

func TestSelfToolScratchpadKeyLimit(t *testing.T) {
	tool := NewSelfTool(&selfToolTestRuntime{view: map[string]any{}}, true)
	ctx := selfTestContext("session-limit")
	for i := 0; i < maxSelfRuntimeKeys; i++ {
		if result := runSelfTool(tool, ctx, "set", fmt.Sprintf("key-%d", i), i, true); result.IsError {
			t.Fatalf("set key %d: %s", i, result.ForLLM)
		}
	}
	if result := runSelfTool(tool, ctx, "set", "overflow", true, true); !result.IsError {
		t.Fatal("scratchpad accepted more than 64 keys")
	}
}

func TestSelfToolScratchpadLimitsNoteSizeAndAllowsDelete(t *testing.T) {
	tool := NewSelfTool(&selfToolTestRuntime{view: map[string]any{}}, true)
	ctx := selfTestContext("session-size")

	// A JSON string adds two quote bytes, so this is exactly at the limit.
	atLimit := strings.Repeat("x", maxSelfNoteBytes-2)
	if result := runSelfTool(tool, ctx, "set", "big", atLimit, true); result.IsError {
		t.Fatalf("set note at the size limit: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "set", "big", atLimit+"x", true); !result.IsError {
		t.Error("scratchpad accepted a value over the size limit")
	}
	longKey := strings.Repeat("k", maxSelfNoteKeyLen+1)
	if result := runSelfTool(tool, ctx, "set", longKey, "v", true); !result.IsError {
		t.Error("scratchpad accepted a key over the length limit")
	}

	for i := 0; i < maxSelfRuntimeKeys-1; i++ {
		if result := runSelfTool(tool, ctx, "set", fmt.Sprintf("key-%d", i), i, true); result.IsError {
			t.Fatalf("set key %d: %s", i, result.ForLLM)
		}
	}
	if result := runSelfTool(tool, ctx, "set", "overflow", true, true); !result.IsError {
		t.Fatal("scratchpad accepted more than 64 keys")
	}
	if result := runSelfTool(tool, ctx, "set", "big", nil, true); result.IsError {
		t.Fatalf("delete note: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "check", "big", nil, false); !result.IsError {
		t.Error("deleted note is still readable")
	}
	if result := runSelfTool(tool, ctx, "set", "overflow", true, true); result.IsError {
		t.Errorf("delete did not free a scratchpad slot: %s", result.ForLLM)
	}
}

func TestSelfAccessTable(t *testing.T) {
	cases := []struct {
		path  string
		level selfAccessLevel
	}{
		{"max_iterations", selfAccessRestricted},
		{"context_window_tokens", selfAccessRestricted},
		{"model_preset", selfAccessRestricted},
		{"model", selfAccessReadOnly},
		{"config", selfAccessReadOnly},
		{"config.build_info", selfAccessReadOnly},
		{"config.voice.elevenlabs_api_key", selfAccessReadOnly},
		{"config.agents.list.0.subagents", selfAccessBlocked},
		{"config.tools.mcp.servers.demo.env", selfAccessBlocked},
		{"config.tools.mcp.discovery", selfAccessReadOnly},
		{"config.session.identity_links", selfAccessBlocked},
		{"config.future_section", selfAccessBlocked},
		{"config.agents.defaults.future_key", selfAccessReadOnly},
		{"scratchpad-plan", selfAccessDefault},
	}
	for _, tc := range cases {
		if got := selfAccessLevelFor(tc.path); got != tc.level {
			t.Errorf("selfAccessLevelFor(%q) = %d, want %d", tc.path, got, tc.level)
		}
	}
	for _, path := range []string{"", ".", "..", "_auth_token", "config.__class__"} {
		if !selfPathDenied(path) {
			t.Errorf("selfPathDenied(%q) = false, want true", path)
		}
	}
	if selfPathDenied("config.tools") {
		t.Error("selfPathDenied rejected a normal path")
	}
}

func nestedSelfValue(depth int) any {
	var value any = "leaf"
	for range depth {
		value = []any{value}
	}
	return value
}

func TestRedactedConfigSnapshotHidesCredentials(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{MaxTokens: 4096, ModelName: "local"}},
		ModelList: []*config.ModelConfig{{
			ModelName:     "local",
			Model:         "openai/local-model",
			APIKeys:       config.SimpleSecureStrings("sk-secret-model-key"),
			CustomHeaders: map[string]string{"Authorization": "Bearer leak"},
		}},
		Voice: config.VoiceConfig{ElevenLabsAPIKey: "elevenlabs-secret"},
		Tools: config.ToolsConfig{
			MCP: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"demo": {Command: "run", Env: map[string]string{
					"OPENAI_API_KEY": "sk-env-secret",
					"PLAIN":          "ok",
				}},
			}},
		},
	}

	view := redactedConfigSnapshot(cfg)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("redacted snapshot is not JSON: %v", err)
	}
	text := string(encoded)
	for _, secret := range []string{"sk-secret-model-key", "elevenlabs-secret", "sk-env-secret", "Bearer leak"} {
		if strings.Contains(text, secret) {
			t.Errorf("snapshot leaked %q: %s", secret, text)
		}
	}

	defaults, _ := view["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens was redacted: %v", defaults["max_tokens"])
	}
	// BLOCKED sections are omitted entirely rather than redacted.
	servers, _ := view["tools"].(map[string]any)["mcp"].(map[string]any)["servers"]
	if servers != nil {
		t.Errorf("blocked mcp.servers leaked into the snapshot: %v", servers)
	}
	// A plaintext secret under a READ_ONLY key is masked, not dropped.
	voice, _ := view["voice"].(map[string]any)
	if voice["elevenlabs_api_key"] != selfRedactedValue {
		t.Errorf("elevenlabs_api_key = %v, want %q", voice["elevenlabs_api_key"], selfRedactedValue)
	}
}

func TestSelfToolBlocksInfrastructureSections(t *testing.T) {
	agent := &AgentInstance{
		ID: "main", modelMu: &sync.RWMutex{}, Tools: tools.NewToolRegistry(), MaxIterations: 20,
	}
	cfg := &config.Config{
		Plugins: config.PluginsConfig{Enabled: true, Entries: map[string]config.PluginEntryConfig{
			"demo": {Enabled: true, Path: "/host/secret/plugin"},
		}},
	}
	tool := NewSelfTool(&agentRuntimeControl{agent: agent, cfg: cfg}, true)
	ctx := selfTestContext("session-1")

	if got := runSelfTool(tool, ctx, "check", "", nil, false).ForLLM; strings.Contains(got, "/host/secret/plugin") {
		t.Fatalf("snapshot leaked a blocked plugin path: %s", got)
	}
	for _, key := range []string{"config.plugins", "config.plugins.entries"} {
		if result := runSelfTool(tool, ctx, "check", key, nil, false); !result.IsError {
			t.Errorf("check %q on a blocked section was allowed", key)
		}
	}
}

func TestSelfToolExposesRedactedConfigReadOnly(t *testing.T) {
	agent := &AgentInstance{
		ID: "main", modelMu: &sync.RWMutex{}, Tools: tools.NewToolRegistry(), MaxIterations: 20,
	}
	cfg := &config.Config{Voice: config.VoiceConfig{ElevenLabsAPIKey: "top-secret"}}
	tool := NewSelfTool(&agentRuntimeControl{agent: agent, cfg: cfg}, true)
	ctx := selfTestContext("session-1")

	got := runSelfTool(tool, ctx, "check", "", nil, false).ForLLM
	if !strings.Contains(got, `"config"`) {
		t.Fatalf("snapshot is missing the config field: %s", got)
	}
	if strings.Contains(got, "top-secret") {
		t.Fatalf("snapshot leaked a credential: %s", got)
	}
	if result := runSelfTool(tool, ctx, "set", "config.voice.elevenlabs_api_key", "x", true); !result.IsError {
		t.Fatal("set on a config path was accepted")
	}
}

func selfReadOnlyRuntime() *selfToolTestRuntime {
	return &selfToolTestRuntime{view: map[string]any{
		"max_iterations":        20,
		"context_window_tokens": 32768,
		"model":                 "local",
		"model_preset":          "local",
		"config":                map[string]any{"version": "1", "hooks": map[string]any{"x": 1}},
		"web_config":            map[string]any{"provider": "duckduckgo", "format": "markdown"},
		"defaults": map[string]any{
			"max_iterations":        20,
			"context_window_tokens": 32768,
			"model":                 "local",
		},
		"session_overrides": map[string]any{"max_iterations": 35},
	}}
}

func TestSelfToolListChildrenCarryAccessAndRange(t *testing.T) {
	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")

	root := runSelfTool(tool, ctx, "list", "", nil, false)
	if root.IsError {
		t.Fatalf("list root: %s", root.ForLLM)
	}
	var rootEntries map[string]map[string]any
	if err := json.Unmarshal([]byte(root.ForLLM), &rootEntries); err != nil {
		t.Fatalf("list root is not JSON: %v\n%s", err, root.ForLLM)
	}
	if got := rootEntries["max_iterations"]["access"]; got != "RESTRICTED" {
		t.Errorf("max_iterations access = %v, want RESTRICTED", got)
	}
	if got := rootEntries["max_iterations"]["range"]; got != "1-100" {
		t.Errorf("max_iterations range = %v, want 1-100", got)
	}
	if got := rootEntries["model"]["access"]; got != "READ_ONLY" {
		t.Errorf("model access = %v, want READ_ONLY", got)
	}

	web := runSelfTool(tool, ctx, "list", "web_config", nil, false)
	if web.IsError {
		t.Fatalf("list web_config: %s", web.ForLLM)
	}
	var webEntries map[string]map[string]any
	if err := json.Unmarshal([]byte(web.ForLLM), &webEntries); err != nil {
		t.Fatalf("list web_config is not JSON: %v\n%s", err, web.ForLLM)
	}
	if got := webEntries["provider"]["access"]; got != "READ_ONLY" {
		t.Errorf("web_config.provider access = %v, want READ_ONLY", got)
	}
}

func TestSelfToolSchemaListsRules(t *testing.T) {
	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")
	result := runSelfTool(tool, ctx, "schema", "", nil, false)
	if result.IsError {
		t.Fatalf("schema: %s", result.ForLLM)
	}
	var rules []map[string]any
	if err := json.Unmarshal([]byte(result.ForLLM), &rules); err != nil {
		t.Fatalf("schema is not JSON: %v\n%s", err, result.ForLLM)
	}
	byPath := map[string]map[string]any{}
	for _, rule := range rules {
		path, _ := rule["path"].(string)
		byPath[path] = rule
	}
	if got := byPath["max_iterations"]["range"]; got != "1-100" {
		t.Errorf("schema max_iterations range = %v, want 1-100", got)
	}
	if got := byPath["config.evolution"]["access"]; got != "BLOCKED" {
		t.Errorf("schema config.evolution access = %v, want BLOCKED", got)
	}
	if got := byPath["model"]["access"]; got != "READ_ONLY" {
		t.Errorf("schema model access = %v, want READ_ONLY", got)
	}
}

func TestSelfToolExplainDescribesPath(t *testing.T) {
	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")

	result := runSelfTool(tool, ctx, "explain", "max_iterations", nil, false)
	if result.IsError {
		t.Fatalf("explain max_iterations: %s", result.ForLLM)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(result.ForLLM), &info); err != nil {
		t.Fatalf("explain is not JSON: %v\n%s", err, result.ForLLM)
	}
	if info["access"] != "RESTRICTED" || info["writable"] != true || info["range"] != "1-100" {
		t.Errorf("explain max_iterations = %v", info)
	}
	if info["value"] != float64(20) {
		t.Errorf("explain max_iterations value = %v, want 20", info["value"])
	}

	model := runSelfTool(tool, ctx, "explain", "model", nil, false)
	var modelInfo map[string]any
	if err := json.Unmarshal([]byte(model.ForLLM), &modelInfo); err != nil {
		t.Fatalf("explain model is not JSON: %v", err)
	}
	if modelInfo["writable"] != false {
		t.Errorf("explain model writable = %v, want false", modelInfo["writable"])
	}

	blocked := runSelfTool(tool, ctx, "explain", "config.hooks", nil, false)
	var blockedInfo map[string]any
	if err := json.Unmarshal([]byte(blocked.ForLLM), &blockedInfo); err != nil {
		t.Fatalf("explain config.hooks is not JSON: %v", err)
	}
	if blockedInfo["access"] != "BLOCKED" {
		t.Errorf("explain config.hooks access = %v, want BLOCKED", blockedInfo["access"])
	}
	if _, leaked := blockedInfo["value"]; leaked {
		t.Errorf("explain config.hooks leaked a value: %v", blockedInfo)
	}
}

func TestSelfToolDiffComparesOverridesWithDefaults(t *testing.T) {
	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")
	result := runSelfTool(tool, ctx, "diff", "", nil, false)
	if result.IsError {
		t.Fatalf("diff: %s", result.ForLLM)
	}
	var diff map[string]map[string]any
	if err := json.Unmarshal([]byte(result.ForLLM), &diff); err != nil {
		t.Fatalf("diff is not JSON: %v\n%s", err, result.ForLLM)
	}
	if diff["max_iterations"]["default"] != float64(20) || diff["max_iterations"]["session"] != float64(35) {
		t.Errorf("diff max_iterations = %v", diff["max_iterations"])
	}

	clean := NewSelfTool(&selfToolTestRuntime{view: map[string]any{
		"defaults":          map[string]any{"max_iterations": 20},
		"session_overrides": map[string]any{},
	}}, false)
	if got := runSelfTool(clean, ctx, "diff", "", nil, false).ForLLM; !strings.Contains(got, "no session overrides") {
		t.Errorf("clean diff = %q, want a no-overrides message", got)
	}
}

func TestSelfToolReadOnlyActionsAllowedWithoutAllowSet(t *testing.T) {
	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")
	for _, action := range []string{"list", "schema", "explain", "diff", "history"} {
		key := ""
		if action == "explain" {
			key = "model"
		}
		if result := runSelfTool(tool, ctx, action, key, nil, false); result.IsError {
			t.Errorf("read-only action %q failed: %s", action, result.ForLLM)
		}
	}
	for _, action := range []string{"set", "reset", "undo"} {
		if result := runSelfTool(tool, ctx, action, "max_iterations", 35, true); !result.IsError {
			t.Errorf("write action %q succeeded in read-only mode", action)
		}
	}
}

func TestSelfToolResetClearsOverrideAndIsUndoable(t *testing.T) {
	runtime := selfReadOnlyRuntime()
	tool := NewSelfTool(runtime, true)
	ctx := selfTestContext("session-1")

	if result := runSelfTool(tool, ctx, "set", "max_iterations", float64(35), true); result.IsError {
		t.Fatalf("set max_iterations: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "reset", "max_iterations", nil, false); result.IsError {
		t.Fatalf("reset max_iterations: %s", result.ForLLM)
	}
	if runtime.resetKey != "max_iterations" {
		t.Fatalf("reset key = %q, want max_iterations", runtime.resetKey)
	}
	if result := runSelfTool(tool, ctx, "reset", "model", nil, false); !result.IsError {
		t.Error("reset accepted the read-only model key")
	}

	history := runSelfTool(tool, ctx, "history", "", nil, false).ForLLM
	if !strings.Contains(history, `"action":"reset"`) || !strings.Contains(history, `"action":"set"`) {
		t.Errorf("history = %s, want set and reset entries", history)
	}
}

func TestSelfToolUndoRevertsScratchpadAndRuntime(t *testing.T) {
	runtime := &selfToolTestRuntime{view: map[string]any{"max_iterations": 20}}
	tool := NewSelfTool(runtime, true)
	ctx := selfTestContext("session-1")

	if result := runSelfTool(tool, ctx, "set", "plan", "first", true); result.IsError {
		t.Fatalf("set plan first: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "set", "plan", "second", true); result.IsError {
		t.Fatalf("set plan second: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "undo", "", nil, false); result.IsError {
		t.Fatalf("undo plan: %s", result.ForLLM)
	}
	if got := runSelfTool(tool, ctx, "check", "plan", nil, false).ForLLM; got != "first" {
		t.Fatalf("plan after undo = %q, want first", got)
	}
	if result := runSelfTool(tool, ctx, "undo", "", nil, false); result.IsError {
		t.Fatalf("undo plan again: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "check", "plan", nil, false); !result.IsError {
		t.Fatal("plan still exists after undoing its creation")
	}

	runtime.setKey, runtime.setValue = "", nil
	if result := runSelfTool(tool, ctx, "set", "max_iterations", float64(35), true); result.IsError {
		t.Fatalf("set max_iterations: %s", result.ForLLM)
	}
	if result := runSelfTool(tool, ctx, "undo", "", nil, false); result.IsError {
		t.Fatalf("undo max_iterations: %s", result.ForLLM)
	}
	if runtime.setKey != "max_iterations" || runtime.setValue != 20 {
		t.Fatalf("undo restored %q=%v, want max_iterations=20", runtime.setKey, runtime.setValue)
	}

	many := NewSelfTool(&selfToolTestRuntime{view: map[string]any{}}, true)
	for i := 0; i < maxSelfHistory+5; i++ {
		if result := runSelfTool(many, ctx, "set", fmt.Sprintf("k%d", i), i, true); result.IsError {
			t.Fatalf("set k%d: %s", i, result.ForLLM)
		}
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(runSelfTool(many, ctx, "history", "", nil, false).ForLLM), &entries); err != nil {
		t.Fatalf("history is not JSON: %v", err)
	}
	if len(entries) != maxSelfHistory {
		t.Fatalf("history length = %d, want %d", len(entries), maxSelfHistory)
	}
}

func TestSelfToolLogsTailsAndRedacts(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "self.log")
	if err := logger.EnableFileLogging(logFile); err != nil {
		t.Fatalf("EnableFileLogging: %v", err)
	}
	defer logger.DisableFileLogging()

	logger.InfoF("hello world", map[string]any{"api_key": "topsecret"})
	logger.ErrorF("boom", nil)

	tool := NewSelfTool(selfReadOnlyRuntime(), false)
	ctx := selfTestContext("session-1")

	all := runSelfTool(tool, ctx, "logs", "", nil, false)
	if all.IsError {
		t.Fatalf("logs: %s", all.ForLLM)
	}
	if !strings.Contains(all.ForLLM, "hello world") || !strings.Contains(all.ForLLM, "boom") {
		t.Errorf("logs missing entries: %s", all.ForLLM)
	}
	if strings.Contains(all.ForLLM, "topsecret") {
		t.Errorf("logs leaked a secret: %s", all.ForLLM)
	}

	errorsOnly := runSelfTool(tool, ctx, "logs", "error", nil, false).ForLLM
	if !strings.Contains(errorsOnly, "boom") || strings.Contains(errorsOnly, "hello world") {
		t.Errorf("logs error filter = %s, want only boom", errorsOnly)
	}
}
