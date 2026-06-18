package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NichlasEk/EutherPunk/internal/config"
)

const defaultSystemPrompt = "Du ar EutherPunk, en lokal AI-agent for kod, konfiguration och praktisk felsokning. Svara pa samma sprak som anvandaren; om anvandaren skriver svenska eller spraket ar oklart, svara pa svenska. Var konkret, fraga innan destruktiva atgarder och prioritera sakra forslag."

const ollamaNumCtx = 4096

//go:embed web/*
var webFiles embed.FS

type serverConfig struct {
	addr           string
	ollamaURL      string
	model          string
	visionModel    string
	configPath     string
	chatDir        string
	downloadsDir   string
	eutherOxideURL string
	voice          config.VoiceConfig
	image          config.ImageConfig
	users          map[string]config.UserConfig
}

type chatRequest struct {
	Message  string        `json:"message"`
	Model    string        `json:"model,omitempty"`
	System   string        `json:"system,omitempty"`
	Images   []string      `json:"images,omitempty"`
	Messages []chatMessage `json:"messages,omitempty"`
}

type chatMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type storedConversation struct {
	ID        string                      `json:"id"`
	User      string                      `json:"user"`
	Title     string                      `json:"title"`
	CreatedAt time.Time                   `json:"created_at"`
	UpdatedAt time.Time                   `json:"updated_at"`
	Messages  []storedConversationMessage `json:"messages"`
}

func (conversation *storedConversation) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string                      `json:"id"`
		User      string                      `json:"user"`
		Title     string                      `json:"title"`
		CreatedAt json.RawMessage             `json:"created_at"`
		UpdatedAt json.RawMessage             `json:"updated_at"`
		Messages  []storedConversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	createdAt, err := optionalJSONTime(raw.CreatedAt)
	if err != nil {
		return fmt.Errorf("created_at: %w", err)
	}
	updatedAt, err := optionalJSONTime(raw.UpdatedAt)
	if err != nil {
		return fmt.Errorf("updated_at: %w", err)
	}
	*conversation = storedConversation{
		ID:        raw.ID,
		User:      raw.User,
		Title:     raw.Title,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		Messages:  raw.Messages,
	}
	return nil
}

type storedConversationMessage struct {
	Role    string                    `json:"role"`
	Content string                    `json:"content"`
	Images  []storedConversationImage `json:"images,omitempty"`
}

type storedConversationImage struct {
	DataURL     string `json:"dataURL,omitempty"`
	URL         string `json:"url,omitempty"`
	Alt         string `json:"alt,omitempty"`
	OllamaImage string `json:"ollamaImage,omitempty"`
}

func optionalJSONTime(raw json.RawMessage) (time.Time, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" || value == `""` {
		return time.Time{}, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

type conversationSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"count"`
}

type ttsRequest struct {
	Text               string `json:"text"`
	Language           string `json:"language,omitempty"`
	VoiceInstruction   string `json:"voice_instruction,omitempty"`
	ModelBackend       string `json:"model_backend,omitempty"`
	MaxChunkCharacters int    `json:"max_chunk_chars,omitempty"`
}

type imageRequest struct {
	Prompt         string        `json:"prompt"`
	NegativePrompt string        `json:"negative_prompt,omitempty"`
	Width          int           `json:"width,omitempty"`
	Height         int           `json:"height,omitempty"`
	Steps          int           `json:"steps,omitempty"`
	Seed           uint64        `json:"seed,omitempty"`
	Context        []chatMessage `json:"context,omitempty"`
}

type imageResponse struct {
	PromptID  string `json:"prompt_id"`
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
	User      string `json:"user"`
	URL       string `json:"url"`
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
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done,omitempty"`
	Error   string        `json:"error,omitempty"`
}

type eutherLinkJob struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
	AudioURL  string `json:"audio_url"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
}

type comfyPromptResponse struct {
	PromptID   string         `json:"prompt_id"`
	NodeErrors map[string]any `json:"node_errors"`
}

type comfyHistoryImage struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type comfyHistoryEntry struct {
	Outputs map[string]struct {
		Images []comfyHistoryImage `json:"images"`
	} `json:"outputs"`
	Status struct {
		StatusStr string `json:"status_str"`
		Completed bool   `json:"completed"`
	} `json:"status"`
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
		visionModel:    envOr("EUTHERPUNK_VISION_MODEL", appConfig.Agent.VisionModel),
		configPath:     appConfig.Path,
		chatDir:        envOr("EUTHERPUNK_CHAT_DIR", defaultChatDirectory(appConfig.Image)),
		downloadsDir:   appConfig.Downloads.Directory,
		eutherOxideURL: appConfig.EutherOxide.UsersURL,
		voice:          appConfig.Voice,
		image:          appConfig.Image,
		users:          appConfig.Users,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleWebIndex())
	mux.HandleFunc("GET /eutherpunk", handleWebIndex())
	mux.HandleFunc("GET /web/{file}", handleWebAsset())
	mux.HandleFunc("GET /api/eutherpunk/status", handleStatus(cfg))
	mux.HandleFunc("GET /api/eutherpunk/models", handleModels(cfg))
	mux.HandleFunc("GET /api/eutherpunk/users", handleUsers(cfg))
	mux.HandleFunc("GET /api/eutherpunk/conversations", handleConversationList(cfg))
	mux.HandleFunc("GET /api/eutherpunk/conversations/{id}", handleConversationGet(cfg))
	mux.HandleFunc("PUT /api/eutherpunk/conversations/{id}", handleConversationPut(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat", handleChat(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat/stream", handleChatStream(cfg))
	mux.HandleFunc("POST /api/eutherpunk/tts", handleTTS(cfg))
	mux.HandleFunc("POST /api/eutherpunk/images/generate", handleImageGenerate(cfg))
	mux.HandleFunc("GET /api/eutherpunk/images/{user}/{file}", handleStoredImage(cfg))
	mux.HandleFunc("GET /downloads/eutherpunk-cli/{platform}", handleCLIDownload(cfg))

	log.Printf("eutherpunkd listening on %s, ollama=%s, model=%s, vision_model=%s", cfg.addr, cfg.ollamaURL, cfg.model, cfg.visionModel)
	if err := http.ListenAndServe(cfg.addr, logRequests(mux)); err != nil {
		log.Fatal(err)
	}
}

func handleStatus(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"service":      "eutherpunk",
			"model":        cfg.model,
			"vision_model": cfg.visionModel,
			"ollama_url":   cfg.ollamaURL,
			"config":       cfg.configPath,
			"chat_dir":     cfg.chatDir,
			"downloads":    cfg.downloadsDir,
			"voice": map[string]any{
				"eutherlink_url": cfg.voice.EutherLinkURL,
				"model_backend":  cfg.voice.ModelBackend,
				"language":       cfg.voice.Language,
			},
			"image": map[string]any{
				"comfyui_url": cfg.image.ComfyUIURL,
				"directory":   cfg.image.Directory,
				"width":       cfg.image.DefaultWidth,
				"height":      cfg.image.DefaultHeight,
				"steps":       cfg.image.DefaultSteps,
			},
			"users": len(cfg.users),
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

func handleConversationList(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		conversations, err := listConversations(cfg.chatDir, user)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user":          user,
			"conversations": conversations,
		})
	}
}

func handleConversationGet(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		id := safeID(r.PathValue("id"))
		if id == "" {
			http.NotFound(w, r)
			return
		}
		conversation, err := readConversation(cfg.chatDir, user, id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, conversation)
	}
}

func handleConversationPut(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		id := safeID(r.PathValue("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("conversation id is required"))
			return
		}
		var conversation storedConversation
		if err := json.NewDecoder(r.Body).Decode(&conversation); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		now := time.Now().UTC()
		conversation.ID = id
		conversation.User = user
		if conversation.CreatedAt.IsZero() {
			conversation.CreatedAt = now
		}
		conversation.UpdatedAt = now
		conversation.Messages = compactStoredMessages(conversation.Messages)
		conversation.Title = conversationTitle(conversation.Title, conversation.Messages)
		if err := writeConversation(cfg.chatDir, conversation); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, conversation)
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
		messages := requestMessages(req)
		if len(messages) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("message is required"))
			return
		}

		model := req.Model
		if model == "" {
			model = chatModel(cfg, messages)
		}
		visionRequest := isVisionRequest(cfg, model, messages)
		messages = messagesForSelectedModel(cfg, model, messages)
		system := req.System
		if system == "" {
			system = systemPromptForMessages(defaultSystemPrompt, messages)
		}

		var answer string
		var err error
		if visionRequest {
			answer, err = askVisionOllama(r.Context(), cfg, system, messages)
		} else {
			answer, err = askOllama(r.Context(), cfg.ollamaURL, model, system, messages)
		}
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
		messages := requestMessages(req)
		if len(messages) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("message is required"))
			return
		}

		model := req.Model
		if model == "" {
			model = chatModel(cfg, messages)
		}
		visionRequest := isVisionRequest(cfg, model, messages)
		messages = messagesForSelectedModel(cfg, model, messages)
		system := req.System
		if system == "" {
			system = systemPromptForMessages(defaultSystemPrompt, messages)
		}

		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if visionRequest {
			answer, err := askVisionOllama(r.Context(), cfg, system, messages)
			encoder := json.NewEncoder(w)
			if err != nil {
				_ = encoder.Encode(streamChunk{Model: model, Error: err.Error(), Done: true})
				return
			}
			if answer != "" {
				_ = encoder.Encode(streamChunk{Model: model, Delta: answer})
			}
			_ = encoder.Encode(streamChunk{Model: model, Done: true})
			return
		}
		if err := streamOllama(r.Context(), w, cfg.ollamaURL, model, system, messages); err != nil {
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

func handleTTS(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ttsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Text = strings.TrimSpace(req.Text)
		if req.Text == "" {
			writeError(w, http.StatusBadRequest, errors.New("text is required"))
			return
		}
		audio, err := synthesizeWithEutherLink(r.Context(), cfg.voice, req)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audio)
	}
}

func handleStoredImage(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := safePathSegment(r.PathValue("user"))
		file := safeImageFileName(r.PathValue("file"))
		if user == "" || file == "" || !strings.HasSuffix(file, ".png") {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(imageDirectory(cfg.image), user, file)
		w.Header().Set("Cache-Control", "private, max-age=31536000")
		http.ServeFile(w, r, path)
	}
}

func handleImageGenerate(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req imageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Prompt = strings.TrimSpace(req.Prompt)
		if req.Prompt == "" {
			writeError(w, http.StatusBadRequest, errors.New("prompt is required"))
			return
		}
		if prompt := imagePromptFromContext(r.Context(), cfg, req); prompt != "" {
			req.Prompt = prompt
		}
		out, err := generateWithComfyUI(r.Context(), cfg.image, requestUser(r, cfg), req)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func imagePromptFromContext(ctx context.Context, cfg serverConfig, req imageRequest) string {
	messages := requestMessages(chatRequest{Messages: req.Context})
	if len(messages) == 0 {
		return req.Prompt
	}
	system := "You convert a chat conversation into one concise English prompt for an image generator. Use the latest user request as the instruction, include relevant visual context from earlier messages or images, and return only the final image prompt with no markdown or explanations."
	messages = append(messages, ollamaMessage{
		Role:    "user",
		Content: "Final image request: " + req.Prompt + "\nWrite the image generation prompt.",
	})
	model := cfg.model
	if cfg.visionModel != "" && messagesHaveImages(messages) {
		model = cfg.visionModel
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	prompt, err := askOllama(ctx, cfg.ollamaURL, model, system, messages)
	if err != nil {
		log.Printf("image prompt context rewrite failed: %v", err)
		return req.Prompt
	}
	prompt = cleanImagePrompt(prompt)
	if prompt == "" {
		return req.Prompt
	}
	return prompt
}

func cleanImagePrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	prompt = strings.Trim(prompt, "` \t\r\n")
	prompt = strings.TrimPrefix(prompt, "json")
	prompt = strings.TrimPrefix(prompt, "text")
	prompt = strings.TrimSpace(prompt)
	prompt = strings.Trim(prompt, `"'`)
	if len(prompt) > 1200 {
		prompt = prompt[:1200]
	}
	return strings.TrimSpace(prompt)
}

func askOllama(ctx context.Context, ollamaURL, model, system string, messages []ollamaMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	payload := ollamaChatRequest{
		Model:    model,
		Stream:   false,
		Messages: append([]ollamaMessage{{Role: "system", Content: system}}, messages...),
		Options: map[string]any{
			"num_ctx":     ollamaNumCtx,
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

func askVisionOllama(ctx context.Context, cfg serverConfig, system string, messages []ollamaMessage) (string, error) {
	answer, err := askOllama(ctx, cfg.ollamaURL, cfg.visionModel, system, messages)
	if err != nil {
		return "", err
	}
	answer = normalizeVisionAnswer(answer)
	if answer != "" {
		return answer, nil
	}
	for _, prompt := range []string{"What animal is shown?", "What is the main animal shown?"} {
		answer, err = askOllama(ctx, cfg.ollamaURL, cfg.visionModel, system, visionFallbackMessages(messages, prompt))
		if err != nil {
			return "", err
		}
		answer = normalizeVisionAnswer(answer)
		if answer != "" {
			return answer, nil
		}
	}
	return "Jag kunde inte tolka bilden med den nuvarande visionmodellen.", nil
}

func visionFallbackMessages(messages []ollamaMessage, prompt string) []ollamaMessage {
	out := make([]ollamaMessage, 0, len(messages))
	for _, message := range messages {
		if len(message.Images) == 0 {
			out = append(out, message)
			continue
		}
		out = append(out, ollamaMessage{
			Role:    message.Role,
			Content: prompt,
			Images:  message.Images,
		})
	}
	return out
}

func normalizeVisionAnswer(answer string) string {
	answer = strings.TrimSpace(strings.ReplaceAll(answer, "\u00a0", " "))
	answer = strings.Trim(answer, ". ")
	switch strings.ToLower(answer) {
	case "monkey", "a monkey":
		return "Det ser ut som en apa."
	case "proboscis monkey", "a proboscis monkey":
		return "Det ser ut som en näsapa."
	case "cat", "a cat":
		return "Det ser ut som en katt."
	case "dog", "a dog":
		return "Det ser ut som en hund."
	case "bird", "a bird":
		return "Det ser ut som en fågel."
	}
	if answer == "" {
		return ""
	}
	return answer
}

func generateWithComfyUI(ctx context.Context, image config.ImageConfig, user string, req imageRequest) (imageResponse, error) {
	var out imageResponse
	baseURL := strings.TrimRight(image.ComfyUIURL, "/")
	if baseURL == "" {
		return out, errors.New("image.comfyui_url is not configured")
	}
	timeout := time.Duration(image.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt, err := buildZImagePrompt(image, req)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/prompt", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("ComfyUI returned %s: %s", resp.Status, string(body))
	}
	var queued comfyPromptResponse
	if err := json.Unmarshal(body, &queued); err != nil {
		return out, err
	}
	if queued.PromptID == "" {
		return out, fmt.Errorf("ComfyUI response missing prompt_id: %s", string(body))
	}
	if len(queued.NodeErrors) > 0 {
		return out, fmt.Errorf("ComfyUI rejected workflow: %v", queued.NodeErrors)
	}

	imageInfo, err := waitForComfyImage(ctx, baseURL, queued.PromptID)
	if err != nil {
		return out, err
	}
	data, err := fetchComfyImage(ctx, baseURL, imageInfo)
	if err != nil {
		return out, err
	}
	storedName, err := storeGeneratedImage(image, user, data)
	if err != nil {
		return out, err
	}
	return imageResponse{
		PromptID:  queued.PromptID,
		Filename:  storedName,
		Subfolder: imageInfo.Subfolder,
		Type:      imageInfo.Type,
		User:      user,
		URL:       "/api/eutherpunk/images/" + url.PathEscape(user) + "/" + url.PathEscape(storedName),
	}, nil
}

func buildZImagePrompt(image config.ImageConfig, req imageRequest) (map[string]any, error) {
	width := clampToStep(defaultInt(req.Width, image.DefaultWidth, 1024), 16, 2048, 16)
	height := clampToStep(defaultInt(req.Height, image.DefaultHeight, 1024), 16, 2048, 16)
	steps := defaultInt(req.Steps, image.DefaultSteps, 8)
	if steps < 1 {
		steps = 1
	}
	if steps > 30 {
		steps = 30
	}
	seed := req.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	negative := strings.TrimSpace(req.NegativePrompt)
	if negative == "" {
		negative = "low quality, blurry, distorted, bad anatomy, extra fingers, text, watermark"
	}

	return map[string]any{
		"1": comfyNode("UNETLoader", map[string]any{
			"unet_name":    "z_image_turbo_bf16.safetensors",
			"weight_dtype": "default",
		}),
		"2": comfyNode("CLIPLoader", map[string]any{
			"clip_name": "qwen_3_4b.safetensors",
			"type":      "lumina2",
			"device":    "default",
		}),
		"3": comfyNode("VAELoader", map[string]any{
			"vae_name": "ZImag-vae.safetensors",
		}),
		"4": comfyNode("CLIPTextEncode", map[string]any{
			"text": req.Prompt,
			"clip": []any{"2", 0},
		}),
		"5": comfyNode("CLIPTextEncode", map[string]any{
			"text": negative,
			"clip": []any{"2", 0},
		}),
		"6": comfyNode("EmptySD3LatentImage", map[string]any{
			"width":      width,
			"height":     height,
			"batch_size": 1,
		}),
		"7": comfyNode("KSampler", map[string]any{
			"model":        []any{"1", 0},
			"positive":     []any{"4", 0},
			"negative":     []any{"5", 0},
			"latent_image": []any{"6", 0},
			"seed":         seed,
			"steps":        steps,
			"cfg":          0.7,
			"sampler_name": "euler",
			"scheduler":    "simple",
			"denoise":      1,
		}),
		"8": comfyNode("VAEDecode", map[string]any{
			"samples": []any{"7", 0},
			"vae":     []any{"3", 0},
		}),
		"9": comfyNode("PreviewImage", map[string]any{
			"images": []any{"8", 0},
		}),
	}, nil
}

func comfyNode(classType string, inputs map[string]any) map[string]any {
	return map[string]any{
		"class_type": classType,
		"inputs":     inputs,
	}
}

func waitForComfyImage(ctx context.Context, baseURL, promptID string) (comfyHistoryImage, error) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return comfyHistoryImage{}, ctx.Err()
		case <-ticker.C:
			body, status, err := proxyGet(ctx, baseURL+"/history/"+url.PathEscape(promptID))
			if err != nil {
				return comfyHistoryImage{}, err
			}
			if status < 200 || status >= 300 {
				return comfyHistoryImage{}, fmt.Errorf("ComfyUI history returned HTTP %d", status)
			}
			var history map[string]comfyHistoryEntry
			if err := json.Unmarshal(body, &history); err != nil {
				return comfyHistoryImage{}, err
			}
			entry, ok := history[promptID]
			if !ok {
				continue
			}
			if entry.Status.Completed && entry.Status.StatusStr != "success" {
				return comfyHistoryImage{}, fmt.Errorf("ComfyUI job finished with status %q", entry.Status.StatusStr)
			}
			for _, output := range entry.Outputs {
				if len(output.Images) > 0 {
					return output.Images[0], nil
				}
			}
		}
	}
}

func fetchComfyImage(ctx context.Context, baseURL string, image comfyHistoryImage) ([]byte, error) {
	values := url.Values{}
	values.Set("filename", image.Filename)
	values.Set("subfolder", image.Subfolder)
	values.Set("type", image.Type)
	body, status, err := proxyGet(ctx, baseURL+"/view?"+values.Encode())
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("ComfyUI image returned HTTP %d", status)
	}
	return body, nil
}

func storeGeneratedImage(image config.ImageConfig, user string, data []byte) (string, error) {
	user = safePathSegment(user)
	if user == "" {
		user = "local"
	}
	dir := filepath.Join(imageDirectory(image), user)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%06d.png", time.Now().Format("20060102-150405"), time.Now().UnixNano()%1000000)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		return "", err
	}
	return name, nil
}

func imageDirectory(image config.ImageConfig) string {
	dir := strings.TrimSpace(image.Directory)
	if dir == "" {
		dir = "var/images"
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Clean(dir)
}

func defaultChatDirectory(image config.ImageConfig) string {
	imageDir := imageDirectory(image)
	base := filepath.Dir(imageDir)
	if base == "." || base == string(filepath.Separator) {
		base = "var"
	}
	return filepath.Join(base, "chats")
}

func listConversations(root, user string) ([]conversationSummary, error) {
	dir := filepath.Join(chatDirectory(root), safePathSegment(user))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []conversationSummary{}, nil
		}
		return nil, err
	}
	out := make([]conversationSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		conversation, err := readConversation(root, user, id)
		if err != nil {
			log.Printf("read conversation %s/%s: %v", user, id, err)
			continue
		}
		out = append(out, conversationSummary{
			ID:        conversation.ID,
			Title:     conversation.Title,
			UpdatedAt: conversation.UpdatedAt,
			CreatedAt: conversation.CreatedAt,
			Count:     len(conversation.Messages),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func readConversation(root, user, id string) (storedConversation, error) {
	var conversation storedConversation
	data, err := os.ReadFile(conversationPath(root, user, id))
	if err != nil {
		return conversation, err
	}
	if err := json.Unmarshal(data, &conversation); err != nil {
		return conversation, err
	}
	conversation.ID = safeID(conversation.ID)
	if conversation.ID == "" {
		conversation.ID = safeID(id)
	}
	conversation.User = safePathSegment(conversation.User)
	if conversation.User == "" {
		conversation.User = safePathSegment(user)
	}
	conversation.Messages = compactStoredMessages(conversation.Messages)
	conversation.Title = conversationTitle(conversation.Title, conversation.Messages)
	return conversation, nil
}

func writeConversation(root string, conversation storedConversation) error {
	dir := filepath.Join(chatDirectory(root), safePathSegment(conversation.User))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, safeID(conversation.ID)+".json")
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(conversation, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func conversationPath(root, user, id string) string {
	return filepath.Join(chatDirectory(root), safePathSegment(user), safeID(id)+".json")
}

func chatDirectory(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "var/chats"
	}
	if filepath.IsAbs(root) {
		return filepath.Clean(root)
	}
	return filepath.Clean(root)
}

func compactStoredMessages(messages []storedConversationMessage) []storedConversationMessage {
	out := messages[:0]
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		message.Role = role
		message.Content = strings.TrimSpace(message.Content)
		message.Images = compactStoredImages(message.Images)
		if message.Content == "" && len(message.Images) == 0 {
			continue
		}
		out = append(out, message)
	}
	return out
}

func compactStoredImages(images []storedConversationImage) []storedConversationImage {
	out := images[:0]
	for _, image := range images {
		image.DataURL = strings.TrimSpace(image.DataURL)
		image.URL = strings.TrimSpace(image.URL)
		image.Alt = strings.TrimSpace(image.Alt)
		image.OllamaImage = strings.TrimSpace(image.OllamaImage)
		if image.DataURL == "" && image.URL == "" && image.OllamaImage == "" {
			continue
		}
		out = append(out, image)
	}
	return out
}

func conversationTitle(current string, messages []storedConversationMessage) string {
	current = strings.TrimSpace(current)
	if current != "" && current != "Ny chat" {
		return truncateTitle(current)
	}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		text := strings.TrimSpace(strings.ReplaceAll(message.Content, "\n", " "))
		if text == "" && len(message.Images) > 0 {
			return "Bildfraga"
		}
		if strings.HasPrefix(strings.ToLower(text), "/bild ") {
			text = "Bild: " + strings.TrimSpace(text[6:])
		}
		if text != "" {
			return truncateTitle(text)
		}
	}
	return "Ny chat"
}

func truncateTitle(value string) string {
	fields := strings.Fields(value)
	if len(fields) > 8 {
		value = strings.Join(fields[:8], " ")
	}
	const maxTitleRunes = 64
	runes := []rune(value)
	if len(runes) > maxTitleRunes {
		return strings.TrimSpace(string(runes[:maxTitleRunes-1])) + "..."
	}
	return value
}

func requestUser(r *http.Request, cfg serverConfig) string {
	for _, header := range []string{"X-EutherOxide-User", "X-Forwarded-User", "X-Remote-User"} {
		if value := safePathSegment(r.Header.Get(header)); value != "" {
			return value
		}
	}
	if len(cfg.users) == 1 {
		for name := range cfg.users {
			return safePathSegment(name)
		}
	}
	return "local"
}

func safeID(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func randomID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func safePathSegment(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func safeImageFileName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func defaultInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func clampToStep(value, min, max, step int) int {
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	if step > 1 {
		value = (value / step) * step
	}
	if value < min {
		return min
	}
	return value
}

func synthesizeWithEutherLink(ctx context.Context, voice config.VoiceConfig, req ttsRequest) ([]byte, error) {
	baseURL := strings.TrimRight(voice.EutherLinkURL, "/")
	if baseURL == "" {
		return nil, errors.New("voice.eutherlink_url is not configured")
	}
	timeout := time.Duration(voice.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	language := req.Language
	if language == "" {
		language = voice.Language
	}
	if language == "" {
		language = "en"
	}
	modelBackend := req.ModelBackend
	if modelBackend == "" {
		modelBackend = voice.ModelBackend
	}
	if modelBackend == "" {
		modelBackend = "grapheneos-matcha-en"
	}
	instruction := req.VoiceInstruction
	if instruction == "" {
		instruction = voice.VoiceInstruction
	}
	if instruction == "" {
		instruction = "A warm, clear English voice with calm natural pacing."
	}
	maxChunkChars := req.MaxChunkCharacters
	if maxChunkChars <= 0 {
		maxChunkChars = 300
	}

	payload := map[string]any{
		"text":              req.Text,
		"voice_instruction": instruction,
		"language":          language,
		"output_format":     "wav",
		"model_backend":     modelBackend,
		"max_chunk_chars":   maxChunkChars,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/tts/jobs", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("EutherLink returned %s: %s", resp.Status, string(body))
	}

	var job eutherLinkJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}
	if job.StatusURL == "" || job.AudioURL == "" {
		return nil, errors.New("EutherLink response missing status_url or audio_url")
	}
	statusURL := absoluteWorkerURL(baseURL, job.StatusURL)
	audioURL := absoluteWorkerURL(baseURL, job.AudioURL)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			status, err := fetchEutherLinkJob(ctx, statusURL)
			if err != nil {
				return nil, err
			}
			switch status.Status {
			case "done":
				if status.AudioURL != "" {
					audioURL = absoluteWorkerURL(baseURL, status.AudioURL)
				}
				return fetchBytes(ctx, audioURL)
			case "failed":
				detail := status.Error
				if detail == "" {
					detail = status.Message
				}
				if detail == "" {
					detail = "EutherLink TTS job failed"
				}
				return nil, errors.New(detail)
			}
		}
	}
}

func fetchEutherLinkJob(ctx context.Context, url string) (eutherLinkJob, error) {
	var job eutherLinkJob
	body, _, err := proxyGet(ctx, url)
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return job, err
	}
	return job, nil
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	body, status, err := proxyGet(ctx, url)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("download returned HTTP %d", status)
	}
	return body, nil
}

func absoluteWorkerURL(baseURL, value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "/") {
		return baseURL + value
	}
	return baseURL + "/" + value
}

func streamOllama(ctx context.Context, w io.Writer, ollamaURL, model, system string, messages []ollamaMessage) error {
	payload := ollamaChatRequest{
		Model:    model,
		Stream:   true,
		Messages: append([]ollamaMessage{{Role: "system", Content: system}}, messages...),
		Options: map[string]any{
			"num_ctx":     ollamaNumCtx,
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

func requestMessages(req chatRequest) []ollamaMessage {
	if len(req.Messages) > 0 {
		out := make([]ollamaMessage, 0, len(req.Messages))
		for _, msg := range req.Messages {
			role := strings.TrimSpace(msg.Role)
			content := strings.TrimSpace(msg.Content)
			if role == "" {
				role = "user"
			}
			if role != "user" && role != "assistant" {
				continue
			}
			if content == "" && len(msg.Images) == 0 {
				continue
			}
			out = append(out, ollamaMessage{
				Role:    role,
				Content: content,
				Images:  compactStrings(msg.Images),
			})
		}
		return out
	}
	if req.Message == "" && len(req.Images) == 0 {
		return nil
	}
	return []ollamaMessage{{
		Role:    "user",
		Content: req.Message,
		Images:  compactStrings(req.Images),
	}}
}

func systemPromptForMessages(base string, messages []ollamaMessage) string {
	if messagesHaveImages(messages) {
		return base + " Nar anvandaren visar en bild, beskriv och resonera om bilden pa anvandarens sprak. Anvand svenska bildord som apa, nasapa och trad nar de passar, men hitta inte pa saker du inte ser."
	}
	return base
}

func chatModel(cfg serverConfig, messages []ollamaMessage) string {
	if cfg.visionModel != "" && messagesHaveImages(messages) {
		return cfg.visionModel
	}
	return cfg.model
}

func isVisionRequest(cfg serverConfig, model string, messages []ollamaMessage) bool {
	return cfg.visionModel != "" && model == cfg.visionModel && messagesHaveImages(messages)
}

func messagesForSelectedModel(cfg serverConfig, model string, messages []ollamaMessage) []ollamaMessage {
	if model != cfg.visionModel || !messagesHaveImages(messages) {
		return messages
	}
	out := make([]ollamaMessage, 0, len(messages))
	for _, message := range messages {
		if len(message.Images) == 0 {
			out = append(out, message)
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content == "" {
			content = "Vad ar det har?"
		}
		out = append(out, ollamaMessage{
			Role:    message.Role,
			Content: "Svara pa svenska. Var kort och konkret. Om du inte kan artbestamma exakt, sag vad du ser och att du ar osaker. Fraga: " + content,
			Images:  message.Images,
		})
	}
	return out
}

func messagesHaveImages(messages []ollamaMessage) bool {
	for _, message := range messages {
		if len(message.Images) > 0 {
			return true
		}
	}
	return false
}

func compactStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
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
