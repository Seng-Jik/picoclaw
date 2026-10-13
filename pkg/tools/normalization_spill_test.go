package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	if strings.Contains(result.ForLLM, "tool output line") {
		t.Fatalf("expected large result omitted from ForLLM, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "saved as a local artifact") {
		t.Fatalf("expected artifact note, got %q", result.ForLLM)
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

func TestExecuteWithContext_SpillFailureStillOmitsContext(t *testing.T) {
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

	if strings.Contains(result.ForLLM, "tool output line") {
		t.Fatalf("expected large result omitted from ForLLM, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "artifact persistence failed") {
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
