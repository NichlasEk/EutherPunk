package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultSystemPrompt = "Du ar EutherPunk, en lokal AI-agent for kod, konfiguration och praktisk felsokning. Var konkret, fraga innan destruktiva atgarder och prioritera sakra forslag."

type serverConfig struct {
	addr      string
	ollamaURL string
	model     string
}

type chatRequest struct {
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	System  string `json:"system,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Message string `json:"message"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []ollamaMessage `json:"messages"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Error   string        `json:"error,omitempty"`
}

func main() {
	cfg := serverConfig{
		addr:      envOr("EUTHERPUNK_ADDR", ":8787"),
		ollamaURL: strings.TrimRight(envOr("OLLAMA_URL", "http://127.0.0.1:11434"), "/"),
		model:     envOr("EUTHERPUNK_MODEL", "qwen3-coder:30b"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/eutherpunk/status", handleStatus(cfg))
	mux.HandleFunc("GET /api/eutherpunk/models", handleModels(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat", handleChat(cfg))

	log.Printf("eutherpunkd listening on %s, ollama=%s, model=%s", cfg.addr, cfg.ollamaURL, cfg.model)
	if err := http.ListenAndServe(cfg.addr, logRequests(mux)); err != nil {
		log.Fatal(err)
	}
}

func handleStatus(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"service":    "eutherpunk",
			"model":      cfg.model,
			"ollama_url": cfg.ollamaURL,
		})
	}
}

func handleModels(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		body, status, err := proxyGet(ctx, cfg.ollamaURL+"/api/tags")
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func handleChat(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			writeError(w, http.StatusBadRequest, errors.New("message is required"))
			return
		}

		model := req.Model
		if model == "" {
			model = cfg.model
		}
		system := req.System
		if system == "" {
			system = defaultSystemPrompt
		}

		answer, err := askOllama(r.Context(), cfg.ollamaURL, model, system, req.Message)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, chatResponse{Model: model, Message: answer})
	}
}

func askOllama(ctx context.Context, ollamaURL, model, system, message string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	payload := ollamaChatRequest{
		Model:  model,
		Stream: false,
		Messages: []ollamaMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: message},
		},
		Options: map[string]any{
			"num_ctx":     32768,
			"temperature": 0.2,
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned %s: %s", resp.Status, string(body))
	}

	var out ollamaChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", errors.New(out.Error)
	}
	return out.Message.Content, nil
}

func proxyGet(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": err.Error(),
	})
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
