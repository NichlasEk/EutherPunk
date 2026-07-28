package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	return validateProposalRuntime(proposal)
}

func validateProposalRuntime(proposal fileProposal) error {
	chrome := firstExecutable(
		"google-chrome-stable",
		"google-chrome",
		"chromium",
		"chromium-browser",
		"chrome",
		"chrome.exe",
	)
	if chrome == "" {
		return nil
	}
	tempDir, err := os.MkdirTemp("", "eutherpunk-browser-check-*")
	if err != nil {
		return fmt.Errorf("kunde inte förbereda webbläsarkontrollen: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var htmlFiles []string
	for _, file := range proposal.Files {
		relative := filepath.Clean(filepath.FromSlash(file.Path))
		target := filepath.Join(tempDir, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("kunde inte förbereda webbläsarkontrollen: %w", err)
		}
		content := file.Content
		switch strings.ToLower(filepath.Ext(relative)) {
		case ".html", ".htm":
			content = injectBrowserSmokeProbe(content)
			htmlFiles = append(htmlFiles, target)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			return fmt.Errorf("kunde inte förbereda webbläsarkontrollen: %w", err)
		}
	}

	for _, htmlPath := range htmlFiles {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		args := []string{
			"--headless=new",
			"--disable-gpu",
			"--disable-background-networking",
			"--disable-component-update",
			"--disable-default-apps",
			"--disable-sync",
			"--no-first-run",
			"--no-default-browser-check",
			"--proxy-server=http://127.0.0.1:9",
			"--proxy-bypass-list=<-loopback>",
			"--virtual-time-budget=1200",
			"--enable-logging=stderr",
			"--v=0",
			"--dump-dom",
			"file://" + filepath.ToSlash(htmlPath),
		}
		command := exec.CommandContext(ctx, chrome, args...)
		var output cappedBuffer
		output.max = 128 * 1024
		command.Stdout = &output
		command.Stderr = &output
		runErr := command.Run()
		cancel()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("webbläsarkontrollen tog för lång tid för %q", filepath.Base(htmlPath))
		}
		if diagnostics := browserRuntimeDiagnostics(output.String(), tempDir); diagnostics != "" {
			return fmt.Errorf("JavaScript-körfel i %q: %s", filepath.Base(htmlPath), diagnostics)
		}
		if runErr != nil && output.Len() == 0 {
			return fmt.Errorf("webbläsarkontrollen kunde inte starta för %q: %v", filepath.Base(htmlPath), runErr)
		}
	}
	return nil
}

func firstExecutable(names ...string) string {
	for _, name := range names {
		if executable, err := exec.LookPath(name); err == nil {
			return executable
		}
	}
	return ""
}

func injectBrowserSmokeProbe(content string) string {
	const probe = `<script>
setTimeout(() => {
  const keys = [["ArrowLeft",37],["ArrowRight",39],["ArrowUp",38],["ArrowDown",40],[" ",32]];
  for (const [key, code] of keys) {
    const event = new KeyboardEvent("keydown", {key, bubbles:true});
    Object.defineProperty(event, "keyCode", {value:code});
    document.dispatchEvent(event);
  }
  document.documentElement.dataset.eutherpunkSmoke = "complete";
}, 50);
</script>`
	lower := strings.ToLower(content)
	if index := strings.LastIndex(lower, "</body"); index >= 0 {
		return content[:index] + probe + content[index:]
	}
	return content + probe
}

func browserRuntimeDiagnostics(output, tempDir string) string {
	var diagnostics []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "CONSOLE:") {
			continue
		}
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "uncaught") &&
			!strings.Contains(lower, "referenceerror") &&
			!strings.Contains(lower, "typeerror") &&
			!strings.Contains(lower, "syntaxerror") &&
			!strings.Contains(lower, "rangeerror") {
			continue
		}
		line = strings.ReplaceAll(strings.TrimSpace(line), filepath.ToSlash(tempDir), "(testarbetsyta)")
		diagnostics = append(diagnostics, line)
		if len(diagnostics) == 4 {
			break
		}
	}
	result := strings.Join(diagnostics, "\n")
	if len(result) > maxValidatorOutputBytes {
		result = result[:maxValidatorOutputBytes]
	}
	return result
}

type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (buffer *cappedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	if remaining := buffer.max - buffer.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = buffer.Buffer.Write(p)
	}
	return originalLength, nil
}

var _ io.Writer = (*cappedBuffer)(nil)

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
