package main

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChatOnlyClientSkipsEutherNetRouting(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/eutherpunk/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")

	answer, handled, err := handleEutherNetForClient(req, serverConfig{}, []ollamaMessage{{
		Role:    "user",
		Content: "/server status",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if handled || answer != "" {
		t.Fatalf("answer = %q, handled = %v", answer, handled)
	}
}

func TestClientContextIsUserLevelAndRequiresChatOnlyMode(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/eutherpunk/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := []ollamaMessage{{Role: "user", Content: "fråga"}}
	if got := messagesWithClientContext(req, "minne", original); len(got) != 1 {
		t.Fatalf("untrusted client context was accepted: %#v", got)
	}

	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	got := messagesWithClientContext(req, "minne", original)
	if len(got) != 2 || got[0].Role != "user" || !strings.Contains(got[0].Content, "minne") {
		t.Fatalf("chat-only context = %#v", got)
	}
	if strings.Contains(got[0].Role, "system") {
		t.Fatalf("client context gained system role: %#v", got[0])
	}
}

func TestClientContextIsCappedAtValidUTF8(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/eutherpunk/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	context := strings.Repeat("å", maxClientContextBytes)
	got := messagesWithClientContext(req, context, nil)
	if len(got) != 1 || !strings.Contains(got[0].Content, "å") {
		t.Fatalf("capped context = %#v", got)
	}
	if !utf8.ValidString(got[0].Content) {
		t.Fatal("capped context is not valid UTF-8")
	}
}
