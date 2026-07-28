package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCLIValuePrefersEnvironment(t *testing.T) {
	t.Setenv("EUTHERPUNK_TEST_VALUE", "from-env")
	got := cliValue("EUTHERPUNK_TEST_VALUE", "from-config", "from-release", false)
	if got != "from-env" {
		t.Fatalf("cliValue = %q", got)
	}
}

func TestCLIValueUsesConfigWhenItExists(t *testing.T) {
	got := cliValue("EUTHERPUNK_TEST_MISSING", "from-config", "from-release", true)
	if got != "from-config" {
		t.Fatalf("cliValue = %q", got)
	}
}

func TestCLIValueUsesEmbeddedReleaseDefaultWithoutConfig(t *testing.T) {
	got := cliValue("EUTHERPUNK_TEST_MISSING", "from-config", "from-release", false)
	if got != "from-release" {
		t.Fatalf("cliValue = %q", got)
	}
}

func TestStreamChatSendsConversationHistory(t *testing.T) {
	var received chatRequest
	originalClient := cliHTTPClient
	cliHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-EutherPunk-Client-Mode"); got != "chat-only" {
			t.Fatalf("client mode header = %q", got)
		}
		if err := json.NewDecoder(req.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
			Body:       io.NopCloser(strings.NewReader("{\"delta\":\"hej\"}\n{\"delta\":\" igen\",\"done\":true}\n")),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() {
		cliHTTPClient = originalClient
	})

	var output bytes.Buffer
	answer, err := streamChat(cliConfig{
		apiURL: "https://example.invalid",
		model:  "test-model",
		memory: memoryState{Enabled: true, Content: "Minns detta."},
	}, []chatMessage{
		{Role: "user", Content: "första"},
		{Role: "assistant", Content: "svaret"},
		{Role: "user", Content: "andra"},
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hej igen" || output.String() != "hej igen" {
		t.Fatalf("answer = %q, output = %q", answer, output.String())
	}
	if received.Model != "test-model" || len(received.Messages) != 3 {
		t.Fatalf("request = %#v", received)
	}
	if received.Messages[0].Content != "/chat första" || received.Messages[2].Content != "/chat andra" {
		t.Fatalf("chat-only messages = %#v", received.Messages)
	}
	if !strings.Contains(received.ClientContext, "Minns detta.") {
		t.Fatalf("client context = %q", received.ClientContext)
	}
}

func TestTrimHistoryKeepsNewestMessages(t *testing.T) {
	messages := make([]chatMessage, 15)
	for i := range messages {
		messages[i].Content = string(rune('a' + i))
	}
	got := trimHistory(messages)
	if len(got) != 12 || got[0].Content != "d" || got[11].Content != "o" {
		t.Fatalf("trimHistory = %#v", got)
	}
}

func TestChatOnlyMessagesNeutralizesServerSlashCommand(t *testing.T) {
	got := chatOnlyMessages([]chatMessage{{Role: "user", Content: "/server status"}})
	if len(got) != 1 || got[0].Content != "/chat /server status" {
		t.Fatalf("chatOnlyMessages = %#v", got)
	}
}

func TestPermissionLevel(t *testing.T) {
	for _, value := range []string{"off", "ask", "session"} {
		got, ok := parsePermissionLevel(value)
		if !ok || string(got) != value {
			t.Fatalf("parsePermissionLevel(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := parsePermissionLevel("always"); ok {
		t.Fatal("permanent permission must not be accepted in preview")
	}
}

func TestSystemReportDoesNotIncludeSensitiveIdentifiers(t *testing.T) {
	report := systemReport{
		OperatingSystem:  "windows",
		OSVersion:        "Windows test",
		Architecture:     "amd64",
		Hostname:         "computer",
		Username:         "user",
		WorkingDirectory: `C:\Test`,
		LogicalCPUs:      8,
		CLIVersion:       "test",
	}
	text := report.String()
	for _, expected := range []string{"windows", "Windows test", "computer", `C:\Test`, "8"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("report missing %q: %s", expected, text)
		}
	}
	for _, forbidden := range []string{"IP-adress", "serienummer", "maskin-ID"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("report unexpectedly contains %q: %s", forbidden, text)
		}
	}
}

func TestCommandSuggestion(t *testing.T) {
	tests := map[string]string{
		"":                      "",
		"hej":                   "",
		"/p":                    "/permissions",
		"/permissions":          "",
		"/permissions s":        "/permissions system ask",
		"/permissions system s": "/permissions system session",
		"/system s":             "/system share",
		"/unknown":              "",
	}
	for input, expected := range tests {
		if got := commandSuggestion(input); got != expected {
			t.Fatalf("commandSuggestion(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestRemoveLastRune(t *testing.T) {
	if got := removeLastRune("hjälp"); got != "hjäl" {
		t.Fatalf("removeLastRune = %q", got)
	}
}

func TestReadTerminalArrowVariants(t *testing.T) {
	tests := map[string]string{
		"\x1b[A":   "up",
		"\x1bOA":   "up",
		"\xe0\x48": "up",
		"\x00\x48": "up",
		"\x1b[B":   "down",
	}
	for encoded, expected := range tests {
		got, err := readTerminalKey(strings.NewReader(encoded))
		if err != nil {
			t.Fatalf("readTerminalKey(%q): %v", encoded, err)
		}
		if got != expected {
			t.Fatalf("readTerminalKey(%q) = %q, want %q", encoded, got, expected)
		}
	}
}

func TestMemoryEnablePersistsAndDisablePreservesFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	state, err := loadMemoryState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled {
		t.Fatal("memory should default to off")
	}
	if err := state.Enable(); err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || !strings.Contains(state.Content, "# EutherPunk Memory") {
		t.Fatalf("enabled state = %#v", state)
	}

	reloaded, err := loadMemoryState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled || reloaded.Content != state.Content {
		t.Fatalf("reloaded state = %#v", reloaded)
	}
	if err := reloaded.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reloaded.Path); err != nil {
		t.Fatalf("memory.md should be preserved: %v", err)
	}
	if _, err := os.Stat(reloaded.EnabledPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enable marker should be removed, got %v", err)
	}
}

func TestMemoryRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(filepath.Join(dir, "memory.md"), bytes.Repeat([]byte("x"), maxMemoryBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.enabled"), []byte("enabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := loadMemoryState(configPath)
	if err == nil {
		t.Fatal("expected oversized memory error")
	}
	if state.Enabled || state.ClientContext() != "" {
		t.Fatal("oversized memory must not be sent")
	}
}
