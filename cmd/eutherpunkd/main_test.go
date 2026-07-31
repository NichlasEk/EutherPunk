package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NichlasEk/EutherPunk/internal/config"
)

func TestImageResourceHandoffUnloadsAllModelRolesAndComfy(t *testing.T) {
	unloaded := make(map[string]bool)
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("Ollama path = %q", r.URL.Path)
		}
		var request struct {
			Model     string `json:"model"`
			KeepAlive int    `json:"keep_alive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.KeepAlive != 0 {
			t.Fatalf("keep_alive = %d", request.KeepAlive)
		}
		unloaded[request.Model] = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()

	if err := releaseOllamaForImage(context.Background(), serverConfig{
		ollamaURL:      ollama.URL,
		model:          "chat-model",
		visionModel:    "vision-model",
		workspaceModel: "workspace-model",
		reviewModel:    "review-model",
	}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{
		"chat-model",
		"vision-model",
		"workspace-model",
		"review-model",
	} {
		if !unloaded[model] {
			t.Fatalf("%s was not unloaded: %#v", model, unloaded)
		}
	}

	var freeRequest map[string]bool
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/free":
			if err := json.NewDecoder(r.Body).Decode(&freeRequest); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(
				`{"devices":[{"type":"cuda","vram_total":25272975360,"vram_free":21474836480}]}`,
			))
		default:
			t.Fatalf("Comfy request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer comfy.Close()
	if err := releaseComfyImageModels(
		context.Background(),
		config.ImageConfig{ComfyUIURL: comfy.URL},
	); err != nil {
		t.Fatal(err)
	}
	if !freeRequest["unload_models"] || !freeRequest["free_memory"] {
		t.Fatalf("Comfy free payload = %#v", freeRequest)
	}
}

func TestWaitForComfyVRAMHeadroomWaitsForAsynchronousRelease(t *testing.T) {
	var requests int
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/system_stats" {
			t.Fatalf("Comfy request = %s %s", r.Method, r.URL.Path)
		}
		requests++
		free := uint64(512 * 1024 * 1024)
		if requests >= 3 {
			free = 6 * 1024 * 1024 * 1024
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"devices": []map[string]any{{
				"type":       "cuda",
				"vram_total": uint64(24 * 1024 * 1024 * 1024),
				"vram_free":  free,
			}},
		})
	}))
	defer comfy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForComfyVRAMHeadroom(
		ctx,
		comfy.URL,
		4*1024*1024*1024,
		time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("system_stats requests = %d, want 3", requests)
	}
}

func TestWaitForComfyVRAMHeadroomFailsClosed(t *testing.T) {
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(
			`{"devices":[{"type":"cuda","vram_total":25272975360,"vram_free":536870912}]}`,
		))
	}))
	defer comfy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err := waitForComfyVRAMHeadroom(
		ctx,
		comfy.URL,
		4*1024*1024*1024,
		time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "kräver minst 4096 MiB") {
		t.Fatalf("error = %v", err)
	}
}

func TestGPUSafetyGateBlocksUntilVerifiedCleanup(t *testing.T) {
	gate := &gpuSafetyGate{}
	if err := gate.check(); err != nil {
		t.Fatal(err)
	}
	gate.block("bara 512 MiB ledigt")
	if err := gate.check(); err == nil || !strings.Contains(err.Error(), "512 MiB") {
		t.Fatalf("blocked error = %v", err)
	}
	gate.clear()
	if err := gate.check(); err != nil {
		t.Fatal(err)
	}
}

func TestGPUSafetyGateWaitsForActiveLocalAI(t *testing.T) {
	gate := &gpuSafetyGate{}
	releaseLocal, err := gate.beginLocalAI()
	if err != nil {
		t.Fatal(err)
	}
	imageAcquired := make(chan struct{})
	go func() {
		releaseImage := gate.beginImage()
		close(imageAcquired)
		gate.clear()
		releaseImage()
	}()

	select {
	case <-imageAcquired:
		t.Fatal("image acquired GPU while local AI still held a shared lease")
	case <-time.After(10 * time.Millisecond):
	}
	releaseLocal()
	select {
	case <-imageAcquired:
	case <-time.After(time.Second):
		t.Fatal("image did not acquire GPU after local AI released it")
	}
	releaseAfter, err := gate.beginLocalAI()
	if err != nil {
		t.Fatal(err)
	}
	releaseAfter()
}

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
		if request.Think == nil || *request.Think {
			t.Fatalf("workspace thinking must be disabled: %#v", request.Think)
		}
		if request.Stream {
			t.Fatal("short structured workspace calls must not stream")
		}
		if workspaceRequestHasProperty(request, "content") {
			if request.Options["num_ctx"] != float64(32768) {
				t.Fatalf("file num_ctx = %#v", request.Options["num_ctx"])
			}
			if request.Options["num_predict"] != float64(12288) {
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
		if request.Options["num_ctx"] != float64(12288) {
			t.Fatalf("planner num_ctx = %#v", request.Options["num_ctx"])
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
