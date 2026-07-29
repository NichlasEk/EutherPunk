package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAskWorkspaceOllamaPlansThenGeneratesFilesSeparately(t *testing.T) {
	var calls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Format == nil {
			t.Fatal("workspace request did not include a JSON schema")
		}
		if request.Options["num_ctx"] != float64(12288) {
			t.Fatalf("num_ctx = %#v", request.Options["num_ctx"])
		}
		if request.Think == nil || *request.Think {
			t.Fatalf("workspace thinking must be disabled: %#v", request.Think)
		}
		if request.Stream {
			t.Fatal("short structured workspace calls must not stream")
		}
		if workspaceRequestHasProperty(request, "content") {
			if request.Options["num_predict"] != float64(6144) {
				t.Fatalf("file num_predict = %#v", request.Options["num_predict"])
			}
			if !strings.Contains(request.Messages[0].Content, "exakt en komplett fil") {
				t.Fatalf("file system prompt = %#v", request.Messages)
			}
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{
					Role:    "assistant",
					Content: `{"content":"print('hej')\n"}`,
				},
				Done: true,
			})
			return
		}
		if request.Options["num_predict"] != float64(768) {
			t.Fatalf("planner num_predict = %#v", request.Options["num_predict"])
		}
		if !strings.Contains(request.Messages[0].Content, "Skriv ingen källkod") {
			t.Fatalf("planner system prompt = %#v", request.Messages)
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{"message":"klart","files":[` +
					`{"path":"main.lua","instruction":"Skriv ett komplett program."}]}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	var progress []string
	message, files, err := askWorkspaceOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "skapa"}},
		func(message string) {
			progress = append(progress, message)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if message != "klart" || len(files) != 1 || files[0].Path != "main.lua" {
		t.Fatalf("message=%q, files=%#v", message, files)
	}
	if calls != 2 || files[0].Content != "print('hej')\n" {
		t.Fatalf("calls=%d files=%#v", calls, files)
	}
	if len(progress) < 3 ||
		!strings.Contains(progress[0], "filplan") ||
		!strings.Contains(progress[len(progress)-1], "filkanalen") {
		t.Fatalf("progress=%#v", progress)
	}
}

func TestAskWorkspaceOllamaHandlesEmptyThinkingResponseWithoutGatewayError(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message:    ollamaMessage{Role: "assistant", Thinking: "unfinished reasoning"},
			Done:       true,
			DoneReason: "length",
			EvalCount:  3072,
		})
	}))
	defer ollama.Close()

	message, files, err := askWorkspaceOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "skapa"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "Inga filer ändrades") || len(files) != 0 {
		t.Fatalf("message=%q, files=%#v", message, files)
	}
}

func TestRequestUserUsesAuthenticatedPrincipalNotHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-EutherPunk-User", "forged-admin")
	req.Header.Set("X-User", "forged-admin")
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, authPrincipal{
		User:     "nichlas",
		AuthMode: "cli_token",
	}))
	if got := requestUser(req, serverConfig{}); got != "nichlas" {
		t.Fatalf("requestUser = %q", got)
	}
}

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
