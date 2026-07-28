package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxValidatorOutputBytes = 4 * 1024

func validateProposalSyntax(proposal fileProposal) error {
	luac, err := exec.LookPath("luac")
	if err != nil {
		return nil
	}
	for _, file := range proposal.Files {
		if !strings.EqualFold(filepath.Ext(file.Path), ".lua") {
			continue
		}
		if err := validateLuaSyntax(luac, file); err != nil {
			return err
		}
	}
	return nil
}

func validateLuaSyntax(luac string, file workspaceFile) error {
	tempDir, err := os.MkdirTemp("", "eutherpunk-lua-check-*")
	if err != nil {
		return fmt.Errorf("kunde inte förbereda Lua-kontrollen: %w", err)
	}
	defer os.RemoveAll(tempDir)

	tempPath := filepath.Join(tempDir, "proposal.lua")
	if err := os.WriteFile(tempPath, []byte(file.Content), 0o600); err != nil {
		return fmt.Errorf("kunde inte förbereda Lua-kontrollen: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, luac, "-p", tempPath)
	output, err := command.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("Lua-kontrollen tog för lång tid för %q", file.Path)
	}
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if len(message) > maxValidatorOutputBytes {
		message = message[:maxValidatorOutputBytes]
	}
	message = strings.ReplaceAll(message, tempPath, filepath.ToSlash(file.Path))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("Lua-syntaxfel i %q: %s", file.Path, message)
}
