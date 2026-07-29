package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectMemorySurvivesAFreshWorkspaceSession(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceState{Root: root}
	task := "Bygg ett spelbart Tetris och fortsätt från tidigare checkpoints."
	if err := ensureProjectMemory(workspace, task); err != nil {
		t.Fatal(err)
	}
	job := workspaceJobResponse{
		ID:    "job-1",
		Model: "devstral-small-2:24b",
	}
	if err := recordProjectJobStarted(workspace, task, job); err != nil {
		t.Fatal(err)
	}
	files := []workspaceFile{{
		Path:    "index.html",
		Content: "<!doctype html><title>Tetris</title>\n",
	}}
	if err := applyWorkspaceDraft(
		workspace,
		files,
		1,
		map[string]bool{},
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}
	job.Status = "completed"
	job.Files = files
	job.DraftFiles = files
	job.DraftRev = 1
	if err := recordProjectJobFinished(
		workspace,
		job,
		"accepted",
		"Syntax and runtime checks passed.",
	); err != nil {
		t.Fatal(err)
	}

	// A new workspaceState represents a fresh CLI session in the same project.
	context, err := workspaceContext(workspaceState{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"PROJEKTSPECIFIKT LÅNGTIDSMINNE",
		task,
		`"status": "accepted"`,
		`"checkpoint_revision": 1`,
		"devstral-small-2:24b",
		"index.html",
		"<!doctype html>",
	} {
		if !strings.Contains(context, expected) {
			t.Fatalf("context missing %q:\n%s", expected, context)
		}
	}
	if strings.Count(context, "--- .eutherpunk/state.json ---") != 1 {
		t.Fatalf("project memory was duplicated:\n%s", context)
	}
}

func TestProjectMemoryIsHarnessOwned(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceState{Root: root}
	if err := ensureProjectMemory(workspace, "test"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		".eutherpunk/project.md",
		".eutherpunk/state.json",
		".eutherpunk/journal.jsonl",
	} {
		if err := validateProposedFile(workspaceFile{Path: path, Content: "overwrite"}); err == nil {
			t.Fatalf("model proposal unexpectedly allowed for %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".eutherpunk", "state.json")); err != nil {
		t.Fatal(err)
	}
}

func TestProjectMemoryRejectsSymlinkedHarnessFile(t *testing.T) {
	root := t.TempDir()
	memoryDir := filepath.Join(root, ".eutherpunk")
	if err := os.Mkdir(memoryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("do not read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(memoryDir, "project.md")); err != nil {
		t.Fatal(err)
	}
	if err := ensureProjectMemory(workspaceState{Root: root}, "test"); err == nil {
		t.Fatal("symlinked project memory file was accepted")
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "do not read\n" {
		t.Fatalf("outside file changed: %q", content)
	}
}

func TestProjectCheckpointPreservesOriginalAcrossRevisions(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceState{Root: root}
	if err := ensureProjectMemory(workspace, "repair"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "main.lua")
	if err := os.WriteFile(target, []byte("print('original')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backedUp := map[string]bool{}
	for revision, content := range []string{"print('broken')\n", "print('fixed')\n"} {
		if err := applyWorkspaceDraft(
			workspace,
			[]workspaceFile{{Path: "main.lua", Content: content}},
			revision+1,
			backedUp,
			io.Discard,
		); err != nil {
			t.Fatal(err)
		}
	}
	backup, err := os.ReadFile(target + ".eutherpunk.previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "print('original')\n" {
		t.Fatalf("backup = %q", backup)
	}
	state, err := loadProjectMemoryState(filepath.Join(root, ".eutherpunk"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "iterating" || state.CheckpointRevision != 2 {
		t.Fatalf("state = %#v", state)
	}
}
