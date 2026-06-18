package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NichlasEk/EutherPunk/internal/config"
)

const defaultSystemPrompt = "Du ar EutherPunk, en lokal AI-agent for kod, konfiguration och praktisk felsokning. Var konkret, fraga innan destruktiva atgarder och prioritera sakra forslag."

//go:embed web/*
var webFiles embed.FS

type serverConfig struct {
	addr           string
	ollamaURL      string
	model          string
	configPath     string
	downloadsDir   string
	eutherOxideURL string
	users          map[string]config.UserConfig
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

type streamChunk struct {
	Model string `json:"model,omitempty"`
	Delta string `json:"delta,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
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
	Done    bool          `json:"done,omitempty"`
	Error   string        `json:"error,omitempty"`
}

func main() {
	appConfig, err := config.Load("")
	if err != nil {
		log.Fatal(err)
	}

	cfg := serverConfig{
		addr:           envOr("EUTHERPUNK_ADDR", appConfig.Agent.Listen),
		ollamaURL:      strings.TrimRight(envOr("OLLAMA_URL", appConfig.Agent.OllamaURL), "/"),
		model:          envOr("EUTHERPUNK_MODEL", appConfig.Agent.Model),
		configPath:     appConfig.Path,
		downloadsDir:   appConfig.Downloads.Directory,
		eutherOxideURL: appConfig.EutherOxide.UsersURL,
		users:          appConfig.Users,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleWebIndex())
	mux.HandleFunc("GET /eutherpunk", handleWebIndex())
	mux.HandleFunc("GET /web/{file}", handleWebAsset())
	mux.HandleFunc("GET /api/eutherpunk/status", handleStatus(cfg))
	mux.HandleFunc("GET /api/eutherpunk/models", handleModels(cfg))
	mux.HandleFunc("GET /api/eutherpunk/users", handleUsers(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat", handleChat(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat/stream", handleChatStream(cfg))
	mux.HandleFunc("GET /downloads/eutherpunk-cli/{platform}", handleCLIDownload(cfg))

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
			"config":     cfg.configPath,
			"downloads":  cfg.downloadsDir,
			"users":      len(cfg.users),
		})
	}
}

func handleWebIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := webFiles.ReadFile("web/index.html")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
}

func handleWebAsset() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.PathValue("file"))
		if name == "." || name == string(filepath.Separator) {
			http.NotFound(w, r)
			return
		}
		data, err := webFiles.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if contentType := mime.TypeByExtension(filepath.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(data)
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

func handleUsers(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"source":            "toml",
			"eutheroxide_users": cfg.eutherOxideURL,
			"users":             cfg.users,
		})
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

func handleChatStream(cfg serverConfig) http.HandlerFunc {
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

		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if err := streamOllama(r.Context(), w, cfg.ollamaURL, model, system, req.Message); err != nil {
			_ = json.NewEncoder(w).Encode(streamChunk{Model: model, Error: err.Error(), Done: true})
		}
	}
}

func handleCLIDownload(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform := filepath.Base(r.PathValue("platform"))
		if platform == "." || strings.Contains(platform, string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		name := "eutherpunk-" + platform
		if strings.Contains(platform, "windows") {
			name += ".exe"
		}
		path := filepath.Join(cfg.downloadsDir, name)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
		http.ServeFile(w, r, path)
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

func streamOllama(ctx context.Context, w io.Writer, ollamaURL, model, system, message string) error {
	payload := ollamaChatRequest{
		Model:  model,
		Stream: true,
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
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama returned %s: %s", resp.Status, string(body))
	}

	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var out ollamaChatResponse
		if err := json.Unmarshal(scanner.Bytes(), &out); err != nil {
			return err
		}
		if out.Error != "" {
			return errors.New(out.Error)
		}
		if out.Message.Content != "" {
			if err := encoder.Encode(streamChunk{Model: model, Delta: out.Message.Content}); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if out.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := encoder.Encode(streamChunk{Model: model, Done: true}); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
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
