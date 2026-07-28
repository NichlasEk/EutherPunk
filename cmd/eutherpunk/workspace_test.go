//go:build !windows

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceSnapshotSkipsSecretsBinaryAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.lua"), []byte("print('hej')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	context, err := workspaceContext(workspaceState{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context, "main.lua") || !strings.Contains(context, "print('hej')") {
		t.Fatalf("snapshot missing safe file: %s", context)
	}
	for _, forbidden := range []string{"TOKEN=secret", "outside-secret", "binary.bin"} {
		if strings.Contains(context, forbidden) {
			t.Fatalf("snapshot contains %q: %s", forbidden, context)
		}
	}
}

func TestFileProposalRejectsTraversalAndProtectedFiles(t *testing.T) {
	for _, path := range []string{"../outside", "/absolute", ".git/config", ".env", "private.key"} {
		answer := "```eutherpunk_files\n" +
			`{"files":[{"path":` + quotedJSON(path) + `,"content":"x"}]}` +
			"\n```"
		if _, _, _, err := parseFileProposal(answer); err == nil {
			t.Fatalf("expected %q to be rejected", path)
		}
	}
}

func TestApprovedProposalWritesOnlyInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	proposal := fileProposal{Files: []workspaceFile{{
		Path:    "game/main.lua",
		Content: "function love.load() end\n",
	}}}
	permissions := sessionPermissions{files: permissionAsk}
	applied, err := approveAndApplyProposal(
		bufio.NewReader(strings.NewReader("ja\n")),
		workspaceState{Root: root},
		&permissions,
		proposal,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("proposal was not applied")
	}
	raw, err := os.ReadFile(filepath.Join(root, "game", "main.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != proposal.Files[0].Content {
		t.Fatalf("content = %q", raw)
	}
}

func TestWorkspaceWriteRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	err := writeWorkspaceFile(root, workspaceFile{Path: "escape/file.txt", Content: "no"})
	if err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file unexpectedly exists: %v", err)
	}
}

func TestApprovedReplacementPreservesPreviousFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.lua")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proposal := fileProposal{Files: []workspaceFile{{Path: "main.lua", Content: "new\n"}}}
	permissions := sessionPermissions{files: permissionAsk}
	applied, err := approveAndApplyProposal(
		bufio.NewReader(strings.NewReader("y\n")),
		workspaceState{Root: root},
		&permissions,
		proposal,
	)
	if err != nil || !applied {
		t.Fatalf("applied=%v, err=%v", applied, err)
	}
	backup, err := os.ReadFile(target + ".eutherpunk.previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old\n" {
		t.Fatalf("backup = %q", backup)
	}
}

func TestParseFileProposalKeepsVisibleAnswer(t *testing.T) {
	answer := "Jag skapar spelet.\n```eutherpunk_files\n" +
		`{"files":[{"path":"main.lua","content":"print('tetris')\n"}]}` +
		"\n```\nKlart."
	proposal, visible, found, err := parseFileProposal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(proposal.Files) != 1 || proposal.Files[0].Path != "main.lua" {
		t.Fatalf("proposal = %#v, found=%v", proposal, found)
	}
	if visible != "Jag skapar spelet.\n\nKlart." {
		t.Fatalf("visible = %q", visible)
	}
}

func quotedJSON(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}
