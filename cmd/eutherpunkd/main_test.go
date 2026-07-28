package main

import (
	"net/http"
	"testing"
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
