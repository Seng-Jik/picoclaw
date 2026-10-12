package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// sessionRuntime holds the per-session overrides the self tool can set. A zero
// value means "follow the configured agent".
type sessionRuntime struct {
	maxIterations int
	contextWindow int
}

func (al *AgentLoop) sessionRuntimeFor(agentID, sessionKey string) sessionRuntime {
	value, _ := al.sessionRuntime.Load(sessionModelKey{agentID: agentID, sessionKey: sessionKey})
	setting, _ := value.(sessionRuntime)
	return setting
}

func (al *AgentLoop) updateSessionRuntime(agentID, sessionKey string, apply func(*sessionRuntime)) {
	key := sessionModelKey{agentID: agentID, sessionKey: sessionKey}
	value, _ := al.sessionRuntime.Load(key)
	setting, _ := value.(sessionRuntime)
	apply(&setting)
	al.sessionRuntime.Store(key, setting)
}

// sessionIterationLimit returns the max_iterations a session set, or 0 if none.
func (al *AgentLoop) sessionIterationLimit(agentID, sessionKey string) int {
	return al.sessionRuntimeFor(agentID, sessionKey).maxIterations
}

// sessionContextWindow returns the context_window_tokens a session set, or 0 if none.
func (al *AgentLoop) sessionContextWindow(agentID, sessionKey string) int {
	return al.sessionRuntimeFor(agentID, sessionKey).contextWindow
}

// agentRuntimeControl is the RuntimeControl the self tool reads and writes
// through. It is bound to the agent instance that owns the tool.
type agentRuntimeControl struct {
	agent *AgentInstance
	cfg   *config.Config
}

func (c *agentRuntimeControl) Snapshot(ctx context.Context) map[string]any {
	if c.agent == nil {
		return map[string]any{}
	}
	base := c.agent
	baseMu := base.modelStateMutex()
	baseMu.RLock()
	defaultMaxIterations := base.MaxIterations
	defaultContextWindow := base.ContextWindow
	defaultModel := base.Model
	baseMu.RUnlock()

	agent := base
	maxIterations := defaultMaxIterations
	contextWindow := defaultContextWindow
	effectiveModel := defaultModel
	if ts := turnStateFromContext(ctx); ts != nil && ts.agent != nil {
		agent = ts.agent
		maxIterations = ts.maxIterations()
		contextWindow = ts.contextWindow()
		effectiveModel = ts.agent.Model
	}
	view := map[string]any{
		"max_iterations":        maxIterations,
		"context_window_tokens": contextWindow,
		"model":                 effectiveModel,
		"model_preset":          effectiveModel,
		"workspace":             agent.Workspace,
		"tool_names":            agent.Tools.List(),
		"request":               requestSelfSnapshot(ctx),
		"turn":                  nil,
		"context_usage":         nil,
		"defaults": map[string]any{
			"max_iterations":        defaultMaxIterations,
			"context_window_tokens": defaultContextWindow,
			"model":                 defaultModel,
		},
		"session_overrides": selfSessionOverrides(
			defaultMaxIterations, defaultContextWindow, defaultModel,
			maxIterations, contextWindow, effectiveModel,
		),
	}
	if ts := turnStateFromContext(ctx); ts != nil && ts.agent != nil {
		info := ts.snapshot()
		view["turn"] = map[string]any{
			"phase":                string(info.Phase),
			"iteration":            info.Iteration,
			"max_iterations":       maxIterations,
			"remaining_iterations": max(0, maxIterations-info.Iteration),
			"elapsed_ms":           max(int64(0), time.Since(info.StartedAt).Milliseconds()),
			"depth":                info.Depth,
		}
		view["context_usage"] = ts.selfContextSnapshot(contextWindow)
	}
	if c.cfg != nil {
		view["web_config"] = selfWebSnapshot(c.cfg)
		view["exec_config"] = selfExecSnapshot(c.cfg)
		view["model_presets"] = selfModelPresets(c.cfg)
		view["config"] = redactedConfigSnapshot(c.cfg)
	}
	return view
}

type selfContextUsage struct {
	usedTokens    int
	outputReserve int
	source        string
}

func (ts *turnState) recordSelfContextUsage(
	messages []providers.Message,
	defs []providers.ToolDefinition,
	usage *providers.UsageInfo,
	options map[string]any,
) {
	budget := &selfContextUsage{source: "local_estimate", outputReserve: ts.agent.MaxTokens}
	if reserve, err := selfInt(options["max_tokens"]); err == nil {
		budget.outputReserve = max(0, reserve)
	}
	if usage != nil && usage.PromptTokens > 0 {
		budget.usedTokens, budget.source = usage.PromptTokens, "provider"
	} else {
		budget.usedTokens = EstimateToolDefsTokens(defs)
		for _, message := range messages {
			budget.usedTokens += EstimateMessageTokens(message)
		}
	}
	ts.mu.Lock()
	ts.selfContextUsage = budget
	ts.mu.Unlock()
}

// The budget describes the last LLM input, before its reply and subsequent tool results.
func (ts *turnState) selfContextSnapshot(window int) map[string]any {
	ts.mu.RLock()
	budget := ts.selfContextUsage
	ts.mu.RUnlock()
	if budget == nil || window <= 0 {
		return nil
	}
	effectiveWindow := max(0, window-budget.outputReserve)
	percent := 0
	if effectiveWindow > 0 {
		percent = min(100, budget.usedTokens*100/effectiveWindow)
	} else if budget.usedTokens > 0 {
		percent = 100
	}
	return map[string]any{
		"used_tokens":           budget.usedTokens,
		"total_tokens":          window,
		"output_reserve_tokens": budget.outputReserve,
		"available_tokens":      max(0, effectiveWindow-budget.usedTokens),
		"used_percent":          percent,
		"source":                budget.source,
	}
}

// selfSessionOverrides reports which session-scoped values differ from the
// configured agent defaults. Only overridden keys are included.
func selfSessionOverrides(
	defaultMaxIterations, defaultContextWindow int, defaultModel string,
	maxIterations, contextWindow int, model string,
) map[string]any {
	overrides := map[string]any{}
	if maxIterations != defaultMaxIterations {
		overrides["max_iterations"] = maxIterations
	}
	if contextWindow != defaultContextWindow {
		overrides["context_window_tokens"] = contextWindow
	}
	if model != defaultModel {
		overrides["model_preset"] = model
	}
	return overrides
}

func (c *agentRuntimeControl) Set(ctx context.Context, key string, value any) error {
	switch key {
	case "model_preset":
		return c.setModelPreset(ctx, value)
	case "context_window_tokens":
		return c.setContextWindow(ctx, value)
	case "max_iterations":
		return c.setMaxIterations(ctx, value)
	default:
		return fmt.Errorf(
			"%s cannot be changed safely through SelfTool; use the agent command or configuration",
			key,
		)
	}
}

func (c *agentRuntimeControl) Reset(ctx context.Context, key string) error {
	switch key {
	case "model_preset":
		ts, al, err := selfSessionTurn(ctx, "model changes")
		if err != nil {
			return err
		}
		al.clearSessionModelOverride(sessionModelKey{agentID: ts.agentID, sessionKey: ts.sessionKey})
		return nil
	case "context_window_tokens":
		ts, al, err := selfSessionTurn(ctx, "context window changes")
		if err != nil {
			return err
		}
		al.updateSessionRuntime(ts.agentID, ts.sessionKey, func(s *sessionRuntime) {
			s.contextWindow = 0
		})
		ts.contextWindowOverride.Store(0)
		return nil
	case "max_iterations":
		ts, al, err := selfSessionTurn(ctx, "iteration limit changes")
		if err != nil {
			return err
		}
		al.updateSessionRuntime(ts.agentID, ts.sessionKey, func(s *sessionRuntime) {
			s.maxIterations = 0
		})
		ts.maxIterationsOverride.Store(0)
		return nil
	default:
		return fmt.Errorf("%s is not a resettable session property", key)
	}
}

func (c *agentRuntimeControl) setModelPreset(ctx context.Context, value any) error {
	preset, ok := value.(string)
	if !ok || strings.TrimSpace(preset) == "" {
		return fmt.Errorf("model_preset must be a non-empty string")
	}
	ts, al, err := selfSessionTurn(ctx, "model changes")
	if err != nil {
		return err
	}
	return al.setSessionModelPreset(ts.agentID, ts.sessionKey, ts.agent, preset)
}

func (c *agentRuntimeControl) setContextWindow(ctx context.Context, value any) error {
	window, err := selfBoundedInt("context_window_tokens", value)
	if err != nil {
		return err
	}
	ts, al, err := selfSessionTurn(ctx, "context window changes")
	if err != nil {
		return err
	}
	al.updateSessionRuntime(ts.agentID, ts.sessionKey, func(s *sessionRuntime) {
		s.contextWindow = window
	})
	ts.contextWindowOverride.Store(int64(window))
	return nil
}

func (c *agentRuntimeControl) setMaxIterations(ctx context.Context, value any) error {
	iterations, err := selfBoundedInt("max_iterations", value)
	if err != nil {
		return err
	}
	ts, al, err := selfSessionTurn(ctx, "iteration limit changes")
	if err != nil {
		return err
	}
	al.updateSessionRuntime(ts.agentID, ts.sessionKey, func(s *sessionRuntime) {
		s.maxIterations = iterations
	})
	ts.maxIterationsOverride.Store(int64(iterations))
	return nil
}

// selfSessionTurn returns the active turn and loop needed for a session-scoped
// write. Turns without a session key share the empty entry, so they are refused.
func selfSessionTurn(ctx context.Context, action string) (*turnState, *AgentLoop, error) {
	ts := turnStateFromContext(ctx)
	al := AgentLoopFromContext(ctx)
	if ts == nil || ts.agent == nil || al == nil {
		return nil, nil, fmt.Errorf("session %s require an active agent turn", action)
	}
	if strings.TrimSpace(ts.sessionKey) == "" {
		return nil, nil, fmt.Errorf("session %s require a session", action)
	}
	return ts, al, nil
}

// selfBoundedInt parses value against the range declared for key in the access
// table. The bool is false when key has no rule.
func selfBoundedInt(key string, value any) (int, error) {
	rule, ok := selfAccessRuleFor(key)
	if !ok {
		return 0, fmt.Errorf("%s is not a writable property", key)
	}
	number, err := selfInt(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if number < rule.min || number > rule.max {
		return 0, selfValidationError(key, rule)
	}
	return number, nil
}

// selfSetMessage is the user-facing confirmation for a runtime write.
func selfSetMessage(key string, value any) string {
	if key == "model_preset" {
		return fmt.Sprintf("model preset for this session will change to %q starting next turn", value)
	}
	return key + " updated"
}

func selfWebSnapshot(cfg *config.Config) map[string]any {
	web := cfg.Tools.Web
	return map[string]any{
		"enabled":           web.Enabled,
		"provider":          web.Provider,
		"prefer_native":     web.PreferNative,
		"fetch_limit_bytes": web.FetchLimitBytes,
		"format":            web.Format,
	}
}

func selfExecSnapshot(cfg *config.Config) map[string]any {
	exec := cfg.Tools.Exec
	return map[string]any{
		"enabled":              exec.Enabled,
		"allow_remote":         exec.AllowRemote,
		"enable_deny_patterns": exec.EnableDenyPatterns,
		"timeout_seconds":      exec.TimeoutSeconds,
	}
}

func selfModelPresets(cfg *config.Config) []string {
	presets := make([]string, 0, len(cfg.ModelList))
	for _, model := range cfg.ModelList {
		if model != nil && strings.TrimSpace(model.ModelName) != "" {
			presets = append(presets, model.ModelName)
		}
	}
	return presets
}

func requestSelfSnapshot(ctx context.Context) map[string]any {
	request := map[string]any{
		"channel":   tools.ToolChannel(ctx),
		"chat_id":   tools.ToolChatID(ctx),
		"sender_id": "",
	}
	if ts := turnStateFromContext(ctx); ts != nil {
		request["channel"] = ts.channel
		request["chat_id"] = ts.chatID
		request["sender_id"] = ts.opts.Dispatch.SenderID()
	}
	return request
}

func validateSelfJSON(value any, depth int) error {
	if depth > 10 {
		return fmt.Errorf("nesting too deep; maximum is 10 levels")
	}
	switch value := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("value must be JSON-compatible")
		}
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("value must be JSON-compatible")
		}
	case []any:
		for _, item := range value {
			if err := validateSelfJSON(item, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range value {
			if err := validateSelfJSON(item, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported value type %T", value)
	}
	return nil
}

func validateSelfSensitiveKeys(value any) error {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if err := validateSelfSensitiveKeys(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range value {
			if selfSensitiveName(key) {
				return fmt.Errorf("access denied")
			}
			if err := validateSelfSensitiveKeys(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func selfInt(value any) (int, error) {
	switch value := value.(type) {
	case int:
		return value, nil
	case float64:
		if math.Trunc(value) == value && value >= float64(math.MinInt) && value <= float64(math.MaxInt) {
			return int(value), nil
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed), nil
		}
	}
	return 0, fmt.Errorf("expected a whole number")
}

func formatSelfValue(value any) string {
	switch value := value.(type) {
	case nil:
		return "null"
	case string:
		return value
	case map[string]any, []any, []string:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	default:
		return fmt.Sprint(value)
	}
}
