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
	luac, _ := exec.LookPath("luac")
	node, _ := exec.LookPath("node")
	for _, file := range proposal.Files {
		switch strings.ToLower(filepath.Ext(file.Path)) {
		case ".lua":
			if luac != "" {
				if err := validateSyntaxCommand(
					"Lua",
					luac,
					[]string{"-p"},
					"proposal.lua",
					file.Path,
					file.Content,
				); err != nil {
					return err
				}
			}
		case ".js", ".mjs", ".cjs":
			if node != "" {
				if err := validateSyntaxCommand(
					"JavaScript",
					node,
					[]string{"--check"},
					"proposal.js",
					file.Path,
					file.Content,
				); err != nil {
					return err
				}
			}
		case ".html", ".htm":
			if node == "" {
				continue
			}
			for index, script := range inlineHTMLScripts(file.Content) {
				label := fmt.Sprintf("%s <script %d>", filepath.ToSlash(file.Path), index+1)
				if err := validateSyntaxCommand(
					"JavaScript",
					node,
					[]string{"--check"},
					"proposal.js",
					label,
					script,
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateSyntaxCommand(
	language, executable string,
	args []string,
	tempName, displayPath, content string,
) error {
	tempDir, err := os.MkdirTemp("", "eutherpunk-syntax-check-*")
	if err != nil {
		return fmt.Errorf("kunde inte förbereda %s-kontrollen: %w", language, err)
	}
	defer os.RemoveAll(tempDir)

	tempPath := filepath.Join(tempDir, tempName)
	if err := os.WriteFile(tempPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("kunde inte förbereda %s-kontrollen: %w", language, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	commandArgs := append(append([]string(nil), args...), tempPath)
	command := exec.CommandContext(ctx, executable, commandArgs...)
	output, err := command.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s-kontrollen tog för lång tid för %q", language, displayPath)
	}
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if len(message) > maxValidatorOutputBytes {
		message = message[:maxValidatorOutputBytes]
	}
	message = strings.ReplaceAll(message, tempPath, displayPath)
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("%s-syntaxfel i %q: %s", language, displayPath, message)
}

func inlineHTMLScripts(content string) []string {
	lower := strings.ToLower(content)
	var scripts []string
	for offset := 0; offset < len(content); {
		start := strings.Index(lower[offset:], "<script")
		if start < 0 {
			break
		}
		start += offset
		openEnd := strings.Index(lower[start:], ">")
		if openEnd < 0 {
			break
		}
		openEnd += start + 1
		closeStart := strings.Index(lower[openEnd:], "</script")
		if closeStart < 0 {
			break
		}
		closeStart += openEnd
		scripts = append(scripts, content[openEnd:closeStart])
		closeEnd := strings.Index(lower[closeStart:], ">")
		if closeEnd < 0 {
			break
		}
		offset = closeStart + closeEnd + 1
	}
	return scripts
}
