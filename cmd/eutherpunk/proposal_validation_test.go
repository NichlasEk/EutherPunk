package main

import (
	"strings"
	"testing"
)

func TestBrowserRuntimeDiagnosticsFindsUncaughtError(t *testing.T) {
	output := `[1:2:INFO:CONSOLE:7] "Uncaught ReferenceError: missingFunction is not defined", source: file:///tmp/test.html (7)
[1:2:WARNING:something] harmless`
	got := browserRuntimeDiagnostics(output, "/tmp")
	if !strings.Contains(got, "missingFunction") {
		t.Fatalf("diagnostics = %q", got)
	}
}

func TestInjectBrowserSmokeProbeBeforeBody(t *testing.T) {
	got := injectBrowserSmokeProbe("<html><body><canvas></canvas></body></html>")
	if !strings.Contains(got, "eutherpunkSmoke") {
		t.Fatalf("probe missing: %s", got)
	}
	if strings.Index(got, "eutherpunkSmoke") > strings.Index(got, "</body>") {
		t.Fatalf("probe was inserted after body: %s", got)
	}
}

func TestValidateProposalRuntimeCatchesPageLoadError(t *testing.T) {
	if firstExecutable(
		"google-chrome-stable",
		"google-chrome",
		"chromium",
		"chromium-browser",
		"chrome",
		"chrome.exe",
	) == "" {
		t.Skip("Chrome/Chromium is not installed")
	}
	err := validateProposalRuntime(fileProposal{Files: []workspaceFile{{
		Path:    "index.html",
		Content: `<!doctype html><script>missingFunction()</script>`,
	}}})
	if err == nil || !strings.Contains(err.Error(), "missingFunction") {
		t.Fatalf("runtime validation error = %v", err)
	}
}
