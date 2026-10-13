package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func newSpillTool(name string, result *ToolResult) *mockRegistryTool {
	tool := newMockTool(name, "spill test tool")
	tool.result = result
	return tool
}

func TestExecuteWithContext_SpillsLargeResultToArtifact(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(32)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)

	// The full payload must not be inline, but a bounded preview must be.
	if utf8.RuneCountInString(result.ForLLM) >= utf8.RuneCountInString(strings.TrimSpace(large)) {
		t.Fatalf("expected ForLLM reduced, got %d runes", utf8.RuneCountInString(result.ForLLM))
	}
	if !strings.Contains(result.ForLLM, largeResultSpillSentinel) {
		t.Fatalf("expected spill sentinel, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "characters truncated") {
		t.Fatalf("expected honest omission marker, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Full output saved to:") {
		t.Fatalf("expected artifact pointer, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 1 {
		t.Fatalf("expected 1 artifact tag, got %+v", result.ArtifactTags)
	}
	tag := result.ArtifactTags[0]
	const prefix = "[file:"
	if !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, "]") {
		t.Fatalf("expected file artifact tag, got %q", tag)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(tag, prefix), "]")
	wantDir := filepath.Join(workspace, ".artifacts", "tool")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("expected artifact in %q, got %q", wantDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected artifact file to be readable: %v", err)
	}
	if string(data) != strings.TrimSpace(large) {
		t.Fatalf("expected artifact file contents to match source text")
	}
	if content := result.ContentForLLM(); !strings.Contains(content, "Local artifact paths: "+tag) {
		t.Fatalf("expected artifact path exposed to the model, got %q", content)
	}
}

func TestExecuteWithContext_SpillKeepsHeadAndTailPreview(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(4096)

	large := "HEAD_MARKER_ALPHA " + strings.Repeat("the quick brown fox jumps over the lazy dog\n", 500) + "TAIL_MARKER_OMEGA"
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)

	if !strings.Contains(result.ForLLM, "HEAD_MARKER_ALPHA") {
		t.Fatalf("expected head preview retained, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "TAIL_MARKER_OMEGA") {
		t.Fatalf("expected tail preview retained, got %q", result.ForLLM)
	}
	if utf8.RuneCountInString(result.ForLLM) >= utf8.RuneCountInString(strings.TrimSpace(large)) {
		t.Fatalf("expected a bounded preview, got %d runes", utf8.RuneCountInString(result.ForLLM))
	}
	if !strings.Contains(result.ForLLM, "characters truncated") {
		t.Fatalf("expected omission marker, got %q", result.ForLLM)
	}
}

func TestExecuteWithContext_SpillIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(32)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)
	first := result.ForLLM
	firstTag := result.ArtifactTags[0]

	// Simulate a second normalization pass (for example after an AfterTool hook).
	reg.SpillLargeResult(result, "big_tool")

	if result.ForLLM != first {
		t.Fatalf("expected idempotent spill, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 1 || result.ArtifactTags[0] != firstTag {
		t.Fatalf("expected no second artifact, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_SpillKeepsReadFileTruncationGuidance(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(4096)

	// Mimic read_file bytes-mode output: the native truncation/offset hint sits
	// at the start, followed by a large body. The head preview must preserve it.
	large := "[file: big.txt | total: 100000 bytes | read: bytes 0-65535]\n" +
		"[TRUNCATED - file has more content. Call read_file again with offset=1234 to continue.]\n\n" +
		strings.Repeat("body line\n", 5000)

	reg.Register(newSpillTool("read_file", SilentResult(large)))
	result := reg.Execute(context.Background(), "read_file", nil)

	if !strings.Contains(result.ForLLM, "offset=1234") {
		t.Fatalf("expected native read_file offset guidance preserved, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, largeResultSpillSentinel) {
		t.Fatalf("expected spill sentinel, got %q", result.ForLLM)
	}
}

func TestSpillLargeResult_AppliesToPostHookResult(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(64)

	// A hook-provided result that never went through tool execution.
	hookResult := SilentResult(strings.Repeat("hook payload line\n", 100))
	reg.SpillLargeResult(hookResult, "hooked_tool")

	if !strings.Contains(hookResult.ForLLM, largeResultSpillSentinel) {
		t.Fatalf("expected post-hook result spilled, got %q", hookResult.ForLLM)
	}
	if len(hookResult.ArtifactTags) != 1 {
		t.Fatalf("expected one artifact tag, got %+v", hookResult.ArtifactTags)
	}
	source := strings.TrimSpace(strings.Repeat("hook payload line\n", 100))
	if utf8.RuneCountInString(hookResult.ForLLM) >= utf8.RuneCountInString(source) {
		t.Fatalf("expected post-hook result reduced")
	}
}

func TestExecuteWithContext_KeepsSmallResultInline(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(4096)

	reg.Register(newSpillTool("small_tool", SilentResult("short output")))

	result := reg.Execute(context.Background(), "small_tool", nil)

	if result.ForLLM != "short output" {
		t.Fatalf("expected small result untouched, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 0 {
		t.Fatalf("expected no artifact tags, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_SpillDisabledWithoutWorkspace(t *testing.T) {
	reg := NewToolRegistry()
	reg.SetMaxInlineResultRunes(8)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)

	if !strings.Contains(result.ForLLM, "tool output line") {
		t.Fatalf("expected large result inline when workspace is unset, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 0 {
		t.Fatalf("expected no artifact tags, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_SpillDisabledByNonPositiveThreshold(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(0)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)

	if !strings.Contains(result.ForLLM, "tool output line") {
		t.Fatalf("expected large result inline when spill disabled, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 0 {
		t.Fatalf("expected no artifact tags, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_DoesNotSpillResponseHandledResults(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(8)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("send_tool", SilentResult(large).WithResponseHandled()))

	result := reg.Execute(context.Background(), "send_tool", nil)

	if !strings.Contains(result.ForLLM, "tool output line") {
		t.Fatalf("expected handled result left inline, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 0 {
		t.Fatalf("expected no artifact tags, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_SpillPreservesExistingArtifactTags(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(8)

	large := strings.Repeat("tool output line\n", 50)
	toolResult := SilentResult(large)
	toolResult.ArtifactTags = []string{"[file:/tmp/existing.png]"}
	reg.Register(newSpillTool("big_tool", toolResult))

	result := reg.Execute(context.Background(), "big_tool", nil)

	if len(result.ArtifactTags) != 2 {
		t.Fatalf("expected existing tag preserved plus new tag, got %+v", result.ArtifactTags)
	}
	if result.ArtifactTags[0] != "[file:/tmp/existing.png]" {
		t.Fatalf("expected original tag first, got %+v", result.ArtifactTags)
	}
}

func TestExecuteWithContext_SpillFailureKeepsPreview(t *testing.T) {
	workspaceRoot := t.TempDir()
	workspaceFile := filepath.Join(workspaceRoot, "not-a-directory")
	if err := os.WriteFile(workspaceFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to create workspace file: %v", err)
	}

	reg := NewToolRegistry()
	reg.SetWorkspace(workspaceFile)
	reg.SetMaxInlineResultRunes(8)

	large := strings.Repeat("tool output line\n", 50)
	reg.Register(newSpillTool("big_tool", SilentResult(large)))

	result := reg.Execute(context.Background(), "big_tool", nil)

	if utf8.RuneCountInString(result.ForLLM) >= utf8.RuneCountInString(strings.TrimSpace(large)) {
		t.Fatalf("expected ForLLM reduced on persistence failure, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, largeResultSpillSentinel) {
		t.Fatalf("expected spill sentinel on failure path, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "persistence failed") {
		t.Fatalf("expected persistence failure note, got %q", result.ForLLM)
	}
	if len(result.ArtifactTags) != 0 {
		t.Fatalf("expected no artifact tags on persistence failure, got %+v", result.ArtifactTags)
	}
}

func TestToolRegistry_CloneCopiesSpillSettings(t *testing.T) {
	workspace := t.TempDir()
	reg := NewToolRegistry()
	reg.SetWorkspace(workspace)
	reg.SetMaxInlineResultRunes(1234)

	clone := reg.Clone()
	if clone.workspace != workspace {
		t.Fatalf("clone workspace = %q, want %q", clone.workspace, workspace)
	}
	if clone.maxInlineResultRunes != 1234 {
		t.Fatalf("clone maxInlineResultRunes = %d, want 1234", clone.maxInlineResultRunes)
	}
}
