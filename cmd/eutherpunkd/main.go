package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NichlasEk/EutherPunk/internal/config"
)

const defaultSystemPrompt = "Du ar EutherPunk, en lokal AI-agent for kod, konfiguration och praktisk felsokning. Svara pa samma sprak som anvandaren; om anvandaren skriver svenska eller spraket ar oklart, svara pa svenska. Var konkret, fraga innan destruktiva atgarder och prioritera sakra forslag."

const ollamaNumCtx = 4096

const (
	safeImageDefaultWidth  = 512
	safeImageDefaultHeight = 512
	safeImageDefaultSteps  = 4
	maxComfySeed           = 1<<31 - 1
	senseNovaGGUF          = "SenseNova-U1-8B-MoT-8step-Q4_K_S.gguf"
	senseNovaLoRA          = "SenseNova-U1-8B-MoT-LoRA-8step-V1.0.safetensors"
)

//go:embed web/*
var webFiles embed.FS

var (
	imageGenerationMu sync.Mutex
	imageJobsMu       sync.Mutex
	imageJobs         = map[string]imageJob{}
)

type serverConfig struct {
	addr           string
	ollamaURL      string
	model          string
	visionModel    string
	configPath     string
	chatDir        string
	settingsDir    string
	promptsPath    string
	downloadsDir   string
	eutherOxideURL string
	eutherNet      config.EutherNetConfig
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

type promptSettings struct {
	DefaultSystem             string `json:"default_system"`
	VisionSystemSuffix        string `json:"vision_system_suffix"`
	VisionFallbackBrief       string `json:"vision_fallback_brief"`
	VisionFallbackSubject     string `json:"vision_fallback_subject"`
	VisionMetadataSystem      string `json:"vision_metadata_system"`
	VisionMetadataUser        string `json:"vision_metadata_user"`
	VisionAnswerSystem        string `json:"vision_answer_system"`
	VisionAnswerUserPrefix    string `json:"vision_answer_user_prefix"`
	ImageToolSystemSuffix     string `json:"image_tool_system_suffix"`
	ImageContextRewriteSystem string `json:"image_context_rewrite_system"`
	ImageContextRewriteUser   string `json:"image_context_rewrite_user"`
	HiddenImageMemoryTemplate string `json:"hidden_image_memory_template"`
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
	Description string `json:"description,omitempty"`
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
	ImageModel     string        `json:"image_model,omitempty"`
	Lora           string        `json:"lora,omitempty"`
	SourceImage    string        `json:"source_image,omitempty"`
	Context        []chatMessage `json:"context,omitempty"`
}

type userSettings struct {
	ChatModel          string   `json:"chat_model"`
	VisionModel        string   `json:"vision_model"`
	ImageModel         string   `json:"image_model"`
	ImageLora          string   `json:"image_lora"`
	VoiceBackend       string   `json:"voice_backend"`
	TTSEnabled         bool     `json:"tts_enabled"`
	ServerVoiceEnabled bool     `json:"server_voice_enabled"`
	Loras              []string `json:"loras,omitempty"`
}

type imageResponse struct {
	PromptID  string `json:"prompt_id"`
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
	User      string `json:"user"`
	URL       string `json:"url"`
}

type imageJob struct {
	ID        string        `json:"job_id"`
	Status    string        `json:"status"`
	Message   string        `json:"message,omitempty"`
	Response  imageResponse `json:"image,omitempty"`
	Error     string        `json:"error,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Message string `json:"message"`
}

type streamChunk struct {
	Model         string `json:"model,omitempty"`
	Delta         string `json:"delta,omitempty"`
	ImageMetadata string `json:"image_metadata,omitempty"`
	Done          bool   `json:"done,omitempty"`
	Error         string `json:"error,omitempty"`
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Stream    bool            `json:"stream"`
	Messages  []ollamaMessage `json:"messages"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive any             `json:"keep_alive,omitempty"`
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
		settingsDir:    envOr("EUTHERPUNK_SETTINGS_DIR", defaultSettingsDirectory(appConfig.Image)),
		promptsPath:    envOr("EUTHERPUNK_PROMPTS_PATH", defaultPromptsPath(envOr("EUTHERPUNK_SETTINGS_DIR", defaultSettingsDirectory(appConfig.Image)))),
		downloadsDir:   appConfig.Downloads.Directory,
		eutherOxideURL: appConfig.EutherOxide.UsersURL,
		eutherNet:      appConfig.EutherNet,
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
	mux.HandleFunc("GET /api/eutherpunk/settings", handleSettingsGet(cfg))
	mux.HandleFunc("PUT /api/eutherpunk/settings", handleSettingsPut(cfg))
	mux.HandleFunc("GET /api/eutherpunk/admin/prompts", handlePromptsGet(cfg))
	mux.HandleFunc("PUT /api/eutherpunk/admin/prompts", handlePromptsPut(cfg))
	mux.HandleFunc("GET /api/eutherpunk/conversations", handleConversationList(cfg))
	mux.HandleFunc("GET /api/eutherpunk/conversations/{id}", handleConversationGet(cfg))
	mux.HandleFunc("PUT /api/eutherpunk/conversations/{id}", handleConversationPut(cfg))
	mux.HandleFunc("DELETE /api/eutherpunk/conversations/{id}", handleConversationDelete(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat", handleChat(cfg))
	mux.HandleFunc("POST /api/eutherpunk/chat/stream", handleChatStream(cfg))
	mux.HandleFunc("POST /api/eutherpunk/tts", handleTTS(cfg))
	mux.HandleFunc("POST /api/eutherpunk/images/generate", handleImageGenerate(cfg))
	mux.HandleFunc("GET /api/eutherpunk/images/jobs/{id}", handleImageJobGet())
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
			"settings_dir": cfg.settingsDir,
			"downloads":    cfg.downloadsDir,
			"euthernet": map[string]any{
				"enabled": cfg.eutherNet.Enabled,
				"url":     cfg.eutherNet.URL,
			},
			"voice": map[string]any{
				"eutherlink_url": cfg.voice.EutherLinkURL,
				"model_backend":  cfg.voice.ModelBackend,
				"language":       cfg.voice.Language,
			},
			"image": map[string]any{
				"comfyui_url":       cfg.image.ComfyUIURL,
				"directory":         cfg.image.Directory,
				"configured_width":  cfg.image.DefaultWidth,
				"configured_height": cfg.image.DefaultHeight,
				"configured_steps":  cfg.image.DefaultSteps,
				"default_width":     defaultImageDimension(0, cfg.image.DefaultWidth, safeImageDefaultWidth),
				"default_height":    defaultImageDimension(0, cfg.image.DefaultHeight, safeImageDefaultHeight),
				"default_steps":     defaultImageSteps(0, cfg.image.DefaultSteps),
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

func handleSettingsGet(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		settings, err := readUserSettings(cfg, user)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		settings.Loras = knownLoras(settings.ImageLora)
		senseNovaLabel := "SenseNova U1 8B"
		if err := ensureSenseNovaReady(r.Context(), cfg.image, "none"); err != nil {
			senseNovaLabel = "SenseNova U1 8B (laddar)"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user":     user,
			"settings": settings,
			"image_models": []map[string]string{
				{"id": "z-image-turbo", "label": "Z-Image Turbo"},
				{"id": "sensenova-u1-8b-fast", "label": senseNovaLabel + " snabb"},
				{"id": "sensenova-u1-8b", "label": senseNovaLabel},
			},
		})
	}
}

func handleSettingsPut(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		settings, err := readUserSettings(cfg, user)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		var incoming userSettings
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		mergeUserSettings(&settings, incoming)
		if err := writeUserSettings(cfg.settingsDir, user, settings); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		var imageModelSyncError string
		if strings.TrimSpace(incoming.ImageModel) != "" {
			if err := syncImageModelControl(r.Context(), cfg.image, settings.ImageModel); err != nil {
				imageModelSyncError = err.Error()
				log.Printf("image model control sync failed for %s: %v", settings.ImageModel, err)
			}
		}

		settings.Loras = knownLoras(settings.ImageLora)
		response := map[string]any{
			"user":     user,
			"settings": settings,
		}
		if imageModelSyncError != "" {
			response["image_model_sync_error"] = imageModelSyncError
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func handlePromptsGet(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prompts, raw, err := readPromptSettings(cfg.promptsPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path":    cfg.promptsPath,
			"prompts": prompts,
			"toml":    raw,
		})
	}
}

func handlePromptsPut(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TOML string `json:"toml"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		raw := strings.TrimSpace(req.TOML)
		if raw == "" {
			writeError(w, http.StatusBadRequest, errors.New("toml is required"))
			return
		}
		if _, err := parsePromptSettings(raw); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := os.MkdirAll(filepath.Dir(cfg.promptsPath), 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := os.WriteFile(cfg.promptsPath, []byte(raw+"\n"), 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		prompts, _, err := readPromptSettings(cfg.promptsPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path":    cfg.promptsPath,
			"prompts": prompts,
			"toml":    raw,
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

func handleConversationDelete(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r, cfg)
		id := safeID(r.PathValue("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("conversation id is required"))
			return
		}
		if err := deleteConversation(cfg.chatDir, user, id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
		if answer, handled, err := handleEutherNetSlash(r.Context(), cfg, lastUserMessage(messages)); handled {
			if err != nil {
				writeError(w, http.StatusBadGateway, err)
				return
			}
			writeJSON(w, http.StatusOK, chatResponse{Model: "euthernet", Message: answer})
			return
		}

		settings, err := readUserSettings(cfg, requestUser(r, cfg))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		prompts, _, err := readPromptSettings(cfg.promptsPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		model := selectedChatModel(settings, req.Model, messages)
		visionRequest := isVisionRequest(settings, model, messages)
		messages = messagesForSelectedModel(settings, model, messages)
		system := req.System
		if system == "" {
			system = systemPromptForMessages(prompts, messages)
		}
		if !visionRequest {
			system = systemPromptWithImageTool(system, prompts)
		}

		var answer string
		if visionRequest {
			answer, err = askVisionOllama(r.Context(), cfg, prompts, system, messages)
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
		if answer, handled, err := handleEutherNetSlash(r.Context(), cfg, lastUserMessage(messages)); handled {
			w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			encoder := json.NewEncoder(w)
			if err != nil {
				_ = encoder.Encode(streamChunk{Model: "euthernet", Error: err.Error(), Done: true})
				return
			}
			if answer != "" {
				_ = encoder.Encode(streamChunk{Model: "euthernet", Delta: answer})
			}
			_ = encoder.Encode(streamChunk{Model: "euthernet", Done: true})
			return
		}

		settings, err := readUserSettings(cfg, requestUser(r, cfg))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		prompts, _, err := readPromptSettings(cfg.promptsPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		model := selectedChatModel(settings, req.Model, messages)
		visionRequest := isVisionRequest(settings, model, messages)
		messages = messagesForSelectedModel(settings, model, messages)
		system := req.System
		if system == "" {
			system = systemPromptForMessages(prompts, messages)
		}
		if !visionRequest {
			system = systemPromptWithImageTool(system, prompts)
		}

		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if visionRequest {
			answer, metadata, err := askVisionOllamaDetailed(r.Context(), cfg, prompts, system, messages)
			encoder := json.NewEncoder(w)
			if err != nil {
				_ = encoder.Encode(streamChunk{Model: model, Error: err.Error(), Done: true})
				return
			}
			if answer != "" {
				_ = encoder.Encode(streamChunk{Model: model, Delta: answer})
			}
			if metadata != "" {
				_ = encoder.Encode(streamChunk{Model: model, ImageMetadata: metadata})
			}
			_ = encoder.Encode(streamChunk{Model: model, Done: true})
			return
		}
		if err := streamOllama(r.Context(), w, cfg.ollamaURL, model, system, messages); err != nil {
			_ = json.NewEncoder(w).Encode(streamChunk{Model: model, Error: err.Error(), Done: true})
		}
	}
}

func lastUserMessage(messages []ollamaMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return strings.TrimSpace(messages[i].Content)
		}
	}
	return ""
}

func handleEutherNetSlash(ctx context.Context, cfg serverConfig, message string) (string, bool, error) {
	message = strings.TrimSpace(message)
	if routed, ok := naturalEutherNetRoute(message); ok {
		message = routed
	}
	if !strings.HasPrefix(strings.ToLower(message), "/server") {
		return "", false, nil
	}
	if !cfg.eutherNet.Enabled {
		return "", true, errors.New("EutherNet ar inte aktiverat i EutherPunk config")
	}
	baseURL := strings.TrimRight(cfg.eutherNet.URL, "/")
	if baseURL == "" {
		return "", true, errors.New("EutherNet URL saknas i EutherPunk config")
	}

	args := strings.Fields(message)
	if len(args) == 1 {
		return eutherNetHelp(), true, nil
	}
	command := strings.ToLower(args[1])
	if command == "full" && len(args) >= 3 && strings.EqualFold(args[2], "report") {
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/report")
		if err != nil {
			return "", true, err
		}
		return eutherNetReport(body), true, nil
	}
	switch command {
	case "status", "health":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/status")
		if err != nil {
			return "", true, err
		}
		return summarizeEutherNetStatus(body), true, nil
	case "repos", "repo", "git":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/repos")
		if err != nil {
			return "", true, err
		}
		return summarizeEutherNetRepos(body), true, nil
	case "summary", "sammanfattning":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/summary")
		if err != nil {
			return "", true, err
		}
		return eutherNetTextField(body, "summary", "EutherNet har ingen summary i senaste snapshoten."), true, nil
	case "changes", "change", "drift", "andringar", "ändringar":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/changes")
		if err != nil {
			return "", true, err
		}
		return eutherNetTextField(body, "changes", "EutherNet ser inga changes i senaste snapshoten."), true, nil
	case "backup", "backup-manifest":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/backup-manifest")
		if err != nil {
			return "", true, err
		}
		return eutherNetTOMLField(body, "manifest_toml", "EutherNet har inget backup-manifest i senaste snapshoten."), true, nil
	case "restore":
		if len(args) >= 3 && (strings.EqualFold(args[2], "drill") || strings.EqualFold(args[2], "övning") || strings.EqualFold(args[2], "ovning")) {
			body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/restore-drill")
			if err != nil {
				return "", true, err
			}
			return eutherNetTOMLField(body, "drill_toml", "EutherNet har ingen restore drill i senaste snapshoten."), true, nil
		}
		if len(args) >= 3 && (strings.EqualFold(args[2], "plan") || strings.EqualFold(args[2], "restore-plan")) {
			body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/restore-plan")
			if err != nil {
				return "", true, err
			}
			return eutherNetTextField(body, "plan", "EutherNet har ingen restore plan i senaste snapshoten."), true, nil
		}
		if len(args) >= 3 && strings.EqualFold(args[2], "bundle") {
			profile := "full"
			if len(args) >= 4 {
				profile = strings.ToLower(args[3])
			}
			body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/restore-bundle?profile="+url.QueryEscape(profile))
			if err != nil {
				return "", true, err
			}
			return eutherNetRestoreBundle(body), true, nil
		}
		return eutherNetHelp(), true, nil
	case "map", "karta":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/map")
		if err != nil {
			return "", true, err
		}
		if len(args) >= 3 && strings.EqualFold(args[2], "prompt") {
			return eutherNetTextField(body, "image_prompt", "EutherNet har ingen bildprompt for serverkartan."), true, nil
		}
		if len(args) >= 3 && (strings.EqualFold(args[2], "image") || strings.EqualFold(args[2], "bild")) {
			return generateEutherNetMapImage(ctx, cfg, body), true, nil
		}
		return eutherNetTOMLField(body, "map_toml", "EutherNet har ingen serverkarta i senaste snapshoten."), true, nil
	case "report", "rapport":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/report")
		if err != nil {
			return "", true, err
		}
		return eutherNetReport(body), true, nil
	case "commands", "kommandon":
		body, err := eutherNetGET(ctx, baseURL+"/api/euthernet/commands")
		if err != nil {
			return "", true, err
		}
		return summarizeEutherNetCommands(body), true, nil
	case "refresh", "inventory", "scan":
		body, err := eutherNetPOST(ctx, baseURL+"/api/euthernet/refresh", map[string]string{})
		if err != nil {
			return "", true, err
		}
		return summarizeEutherNetRefresh(body), true, nil
	case "ask", "fraga", "fråga":
		question := strings.TrimSpace(strings.TrimPrefix(message, args[0]+" "+args[1]))
		if question == "" {
			return "Skriv en fraga efter `/server ask`, till exempel `/server ask vilka repos ar dirty?`.", true, nil
		}
		body, err := eutherNetPOST(ctx, baseURL+"/api/euthernet/ask", map[string]string{"question": question})
		if err != nil {
			return "", true, err
		}
		return eutherNetAnswer(body), true, nil
	case "run", "kor", "kör":
		if len(args) < 3 {
			return "Skriv ett allowlistat kommando efter `/server run`, till exempel `/server run disk`.", true, nil
		}
		body, err := eutherNetPOST(ctx, baseURL+"/api/euthernet/run", map[string]string{"name": args[2]})
		if err != nil {
			return "", true, err
		}
		return summarizeEutherNetRun(body), true, nil
	default:
		return eutherNetHelp(), true, nil
	}
}

func naturalEutherNetRoute(message string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" || strings.HasPrefix(lower, "/") {
		return "", false
	}
	hasMapIntent := strings.Contains(lower, "serverkarta") ||
		strings.Contains(lower, "server karta") ||
		strings.Contains(lower, "eutherverse") ||
		strings.Contains(lower, "karta") ||
		strings.Contains(lower, "diagram") ||
		strings.Contains(lower, "översikt") ||
		strings.Contains(lower, "oversikt") ||
		strings.Contains(lower, "visualisera") ||
		strings.Contains(lower, "visuell") ||
		strings.Contains(lower, "rita") ||
		strings.Contains(lower, "map")
	wantsImage := strings.Contains(lower, "bild") ||
		strings.Contains(lower, "image") ||
		strings.Contains(lower, "rita") ||
		strings.Contains(lower, "generera") ||
		strings.Contains(lower, "skapa") ||
		strings.Contains(lower, "prompt") ||
		strings.Contains(lower, "cyberpunk")
	hasServerContext := strings.Contains(lower, "server") ||
		strings.Contains(lower, "euthernet") ||
		strings.Contains(lower, "eutherverse") ||
		strings.Contains(lower, "eutheroxide") ||
		strings.Contains(lower, "eutherpunk")
	hasRepoContext := strings.Contains(lower, "repo") || strings.Contains(lower, "git")
	if !hasServerContext && !hasRepoContext && !hasMapIntent {
		return "", false
	}
	switch {
	case hasMapIntent:
		if wantsImage {
			return "/server map image", true
		}
		return "/server map", true
	case strings.Contains(lower, "restore drill") || strings.Contains(lower, "restore-drill") || strings.Contains(lower, "restoreövning") || strings.Contains(lower, "restore ovning"):
		return "/server restore drill", true
	case strings.Contains(lower, "backup") && !(strings.Contains(lower, "restore") || strings.Contains(lower, "återställ") || strings.Contains(lower, "aterstall") || strings.Contains(lower, "bootstrap")):
		return "/server backup", true
	case strings.Contains(lower, "backup") && (strings.Contains(lower, "restore") || strings.Contains(lower, "återställ") || strings.Contains(lower, "aterstall") || strings.Contains(lower, "bootstrap")):
		return "/server restore bundle backup", true
	case strings.Contains(lower, "bundle") || strings.Contains(lower, "bootstrap") || strings.Contains(lower, "install script") || strings.Contains(lower, "installationsskript"):
		return "/server restore bundle", true
	case strings.Contains(lower, "restore") || strings.Contains(lower, "återställ") || strings.Contains(lower, "aterstall"):
		return "/server restore plan", true
	case strings.Contains(lower, "full report") || strings.Contains(lower, "serverrapport") || strings.Contains(lower, "rapport"):
		return "/server full report", true
	case strings.Contains(lower, "summary") || strings.Contains(lower, "sammanfatt"):
		return "/server summary", true
	case strings.Contains(lower, "changes") || strings.Contains(lower, "drift") || strings.Contains(lower, "ändring") || strings.Contains(lower, "andring"):
		return "/server changes", true
	case strings.Contains(lower, "repo") || strings.Contains(lower, "git") || strings.Contains(lower, "dirty"):
		return "/server repos", true
	case strings.Contains(lower, "kommando") || strings.Contains(lower, "commands"):
		return "/server commands", true
	case strings.Contains(lower, "hur mår") || strings.Contains(lower, "hur mar") || strings.Contains(lower, "health") || strings.Contains(lower, "status"):
		return "/server summary", true
	default:
		return "", false
	}
}

func eutherNetGET(ctx context.Context, endpoint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return eutherNetDo(req)
}

func eutherNetPOST(ctx context.Context, endpoint string, payload any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return eutherNetDo(req)
}

func eutherNetDo(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("EutherNet svarade %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func eutherNetHelp() string {
	return strings.Join([]string{
		"EutherNet-kommandon:",
		"- `/server status`",
		"- `/server repos`",
		"- `/server summary`",
		"- `/server changes`",
		"- `/server backup`",
		"- `/server restore plan`",
		"- `/server restore drill`",
		"- `/server restore bundle`",
		"- `/server restore bundle backup`",
		"- `/server map`",
		"- `/server map image`",
		"- `/server full report`",
		"- `/server commands`",
		"- `/server refresh`",
		"- `/server ask <fraga>`",
		"- `/server run <allowlist-namn>`",
	}, "\n")
}

func summarizeEutherNetStatus(body []byte) string {
	var payload struct {
		OK           bool              `json:"ok"`
		CollectedAt  string            `json:"collected_at"`
		SSHPreflight bool              `json:"ssh_preflight"`
		Server       map[string]string `json:"server"`
		Collectors   map[string]any    `json:"collectors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	repos := "okand repoantal"
	if collector, ok := payload.Collectors["git_repositories"].(map[string]any); ok {
		if count, ok := collector["repository_count"]; ok {
			repos = fmt.Sprintf("%v repos", count)
		}
	}
	return fmt.Sprintf(
		"EutherNet status: %s samlades %s. SSH=%v, %s.",
		payload.Server["name"],
		payload.CollectedAt,
		payload.SSHPreflight,
		repos,
	)
}

func summarizeEutherNetRepos(body []byte) string {
	var payload struct {
		Repos []struct {
			Path       string `json:"path"`
			Branch     string `json:"branch"`
			Head       string `json:"head"`
			DirtyLines string `json:"dirty_lines"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if len(payload.Repos) == 0 {
		return "EutherNet ser inga repos i senaste snapshoten."
	}
	dirty := []string{}
	for _, repo := range payload.Repos {
		if repo.DirtyLines != "" && repo.DirtyLines != "0" {
			branch := repo.Branch
			if branch == "" {
				branch = "detached/okand"
			}
			dirty = append(dirty, fmt.Sprintf("- `%s` [%s %s], %s statusrader", repo.Path, branch, repo.Head, repo.DirtyLines))
		}
	}
	if len(dirty) == 0 {
		return fmt.Sprintf("EutherNet ser %d repos. Inga dirty repos syns.", len(payload.Repos))
	}
	return fmt.Sprintf("EutherNet ser %d repos. Dirty repos:\n%s", len(payload.Repos), strings.Join(dirty, "\n"))
}

func summarizeEutherNetCommands(body []byte) string {
	var payload struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	lines := []string{"Tillatna EutherNet-kommandon:"}
	for _, command := range payload.Commands {
		lines = append(lines, fmt.Sprintf("- `%s`: %s", command.Name, command.Description))
	}
	return strings.Join(lines, "\n")
}

func summarizeEutherNetRefresh(body []byte) string {
	var payload struct {
		OK           bool   `json:"ok"`
		CollectedAt  string `json:"collected_at"`
		SSHPreflight bool   `json:"ssh_preflight"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if !payload.OK {
		return fmt.Sprintf("Inventory uppdaterades men SSH preflight misslyckades. Tid: %s", payload.CollectedAt)
	}
	return fmt.Sprintf("Inventory uppdaterad %s. SSH preflight ok.", payload.CollectedAt)
}

func eutherNetAnswer(body []byte) string {
	var payload struct {
		Answer string `json:"answer"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if payload.Source != "" {
		return fmt.Sprintf("%s\n\nKalla: EutherNet %s.", payload.Answer, payload.Source)
	}
	return payload.Answer
}

func eutherNetReport(body []byte) string {
	var payload struct {
		Report string `json:"report"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if strings.TrimSpace(payload.Report) == "" {
		return "EutherNet har ingen full report i senaste snapshoten."
	}
	return payload.Report
}

func eutherNetRestoreBundle(body []byte) string {
	var payload struct {
		Profile         string `json:"profile"`
		CollectedAt     string `json:"collected_at"`
		Runbook         string `json:"runbook"`
		BootstrapScript string `json:"bootstrap_script"`
		CodexPrompt     string `json:"codex_prompt"`
		Manifest        struct {
			BasePackages     []string `json:"base_packages"`
			ObservedPackages []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"observed_packages"`
			Repositories []struct {
				Path string `json:"path"`
			} `json:"repositories"`
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if strings.TrimSpace(payload.Runbook) == "" {
		return "EutherNet har ingen restore bundle i senaste snapshoten."
	}
	lines := []string{
		fmt.Sprintf("EutherNet restore bundle `%s` från %s.", payload.Profile, payload.CollectedAt),
		fmt.Sprintf("Baspaket: `%s`.", strings.Join(payload.Manifest.BasePackages, ", ")),
		fmt.Sprintf("Observerade paket i snapshot: `%d`.", len(payload.Manifest.ObservedPackages)),
		fmt.Sprintf("Repos i scope: `%d`.", len(payload.Manifest.Repositories)),
		fmt.Sprintf("Serviceplaner: `%d`.", len(payload.Manifest.Services)),
		"",
		payload.Runbook,
		"## Bootstrap Script",
		"",
		"```sh",
		strings.TrimSpace(payload.BootstrapScript),
		"```",
		"",
		"## Codex Prompt",
		"",
		payload.CodexPrompt,
	}
	return strings.Join(lines, "\n")
}

func eutherNetTextField(body []byte, field string, fallback string) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	value, ok := payload[field].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func eutherNetTOMLField(body []byte, field string, fallback string) string {
	value := eutherNetTextField(body, field, "")
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return "```toml\n" + strings.TrimSpace(value) + "\n```"
}

func generateEutherNetMapImage(ctx context.Context, cfg serverConfig, body []byte) string {
	prompt := eutherNetTextField(body, "image_prompt", "")
	if strings.TrimSpace(prompt) == "" {
		return "EutherNet har ingen bildprompt for serverkartan."
	}
	req := imageRequest{
		Prompt:         prompt,
		NegativePrompt: "blurry text, unreadable labels, logo, watermark, low resolution, cluttered layout",
		Width:          1024,
		Height:         1024,
		Steps:          8,
	}
	user := "server-map"
	job := newImageJob()
	storeImageJob(job)
	go runImageJob(cfg, user, req, job.ID)

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := getImageJob(job.ID)
		if !ok {
			return fmt.Sprintf("Jag startade kartbilden, men tappade jobstatusen. Jobb-ID: `%s`.", job.ID)
		}
		switch current.Status {
		case "done":
			if current.Response.URL != "" {
				return strings.Join([]string{
					"Genererade EutherVerse-serverkartan.",
					"",
					fmt.Sprintf("![EutherVerse serverkarta](%s)", current.Response.URL),
					"",
					current.Response.URL,
				}, "\n")
			}
			return fmt.Sprintf("Kartbilden ar klar, men saknar bild-URL. Jobb-ID: `%s`.", job.ID)
		case "error":
			if current.Error != "" {
				return "Jag hittade serverkartan och startade bildgenereringen, men bildmotorn svarade inte klart:\n\n```text\n" + current.Error + "\n```"
			}
			return "Bildgenereringen misslyckades utan feltext."
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("Genererar EutherVerse-serverkartan. Jobb-ID: `%s`. Status: `/api/eutherpunk/images/jobs/%s`.", job.ID, job.ID)
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Sprintf("Genererar EutherVerse-serverkartan. Jobb-ID: `%s`. Status: `/api/eutherpunk/images/jobs/%s`.", job.ID, job.ID)
}

func summarizeEutherNetRun(body []byte) string {
	var payload struct {
		OK          bool   `json:"ok"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if !payload.OK {
		if payload.Error != "" {
			return "EutherNet kunde inte kora kommandot: " + payload.Error
		}
		if payload.Stderr != "" {
			return "EutherNet kunde inte kora kommandot:\n```text\n" + payload.Stderr + "\n```"
		}
	}
	output := strings.TrimSpace(payload.Stdout)
	if output == "" {
		output = strings.TrimSpace(payload.Stderr)
	}
	if output == "" {
		output = "(tom output)"
	}
	return fmt.Sprintf("EutherNet `%s`:\n```text\n%s\n```", payload.Name, output)
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
		settings, err := readUserSettings(cfg, requestUser(r, cfg))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if strings.TrimSpace(req.ModelBackend) == "" {
			req.ModelBackend = settings.VoiceBackend
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
		user := requestUser(r, cfg)
		job := newImageJob()
		storeImageJob(job)
		go runImageJob(cfg, user, req, job.ID)
		writeJSON(w, http.StatusAccepted, job)
	}
}

func handleImageJobGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := safeID(r.PathValue("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("job id is required"))
			return
		}
		job, ok := getImageJob(id)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("image job not found"))
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func runImageJob(cfg serverConfig, user string, req imageRequest, jobID string) {
	imageGenerationMu.Lock()
	defer imageGenerationMu.Unlock()

	setImageJobStatus(jobID, "running", imageResponse{}, "")
	ctx := context.Background()
	settings, err := readUserSettings(cfg, user)
	if err != nil {
		setImageJobStatus(jobID, "error", imageResponse{}, err.Error())
		return
	}
	if strings.TrimSpace(req.ImageModel) == "" {
		req.ImageModel = settings.ImageModel
	}
	if strings.TrimSpace(req.Lora) == "" {
		req.Lora = settings.ImageLora
	}
	imageModel := normalizeImageModel(req.ImageModel)
	if !isSenseNovaImageModel(imageModel) || imageModel == "sensenova-u1-8b-fast" {
		req.Lora = "none"
	} else if err := ensureSenseNovaReady(ctx, cfg.image, req.Lora); err != nil {
		setImageJobStatus(jobID, "error", imageResponse{}, err.Error())
		return
	}
	if imageModel == "sensenova-u1-8b-fast" {
		if err := ensureSenseNovaReady(ctx, cfg.image, "none"); err != nil {
			setImageJobStatus(jobID, "error", imageResponse{}, err.Error())
			return
		}
	}
	if prompt := imagePromptFromContext(ctx, cfg, req); prompt != "" {
		req.Prompt = prompt
	}
	releaseOllamaForImage(ctx, cfg)
	if isSenseNovaImageModel(imageModel) {
		setImageJobStatusMessage(jobID, "waiting_tts", "Vantar pa Dots TTS innan SenseNova far GPU:n.")
		if err := waitForVoiceResourcesForImage(ctx, cfg); err != nil {
			setImageJobStatus(jobID, "error", imageResponse{}, err.Error())
			return
		}
	}
	setImageJobStatus(jobID, "running", imageResponse{}, "")
	out, err := generateWithComfyUI(ctx, cfg.image, user, req)
	if err != nil {
		setImageJobStatus(jobID, "error", imageResponse{}, err.Error())
		return
	}
	setImageJobStatus(jobID, "done", out, "")
}

func newImageJob() imageJob {
	now := time.Now().UTC()
	return imageJob{
		ID:        randomID(),
		Status:    "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func storeImageJob(job imageJob) {
	imageJobsMu.Lock()
	defer imageJobsMu.Unlock()
	imageJobs[job.ID] = job
	pruneImageJobsLocked(time.Now().UTC().Add(-2 * time.Hour))
}

func getImageJob(id string) (imageJob, bool) {
	imageJobsMu.Lock()
	defer imageJobsMu.Unlock()
	job, ok := imageJobs[id]
	return job, ok
}

func setImageJobStatus(id, status string, response imageResponse, errorText string) {
	imageJobsMu.Lock()
	defer imageJobsMu.Unlock()
	job, ok := imageJobs[id]
	if !ok {
		return
	}
	job.Status = status
	job.Message = ""
	job.Response = response
	job.Error = errorText
	job.UpdatedAt = time.Now().UTC()
	imageJobs[id] = job
}

func setImageJobStatusMessage(id, status, message string) {
	imageJobsMu.Lock()
	defer imageJobsMu.Unlock()
	job, ok := imageJobs[id]
	if !ok {
		return
	}
	job.Status = status
	job.Message = message
	job.Response = imageResponse{}
	job.Error = ""
	job.UpdatedAt = time.Now().UTC()
	imageJobs[id] = job
}

func pruneImageJobsLocked(before time.Time) {
	for id, job := range imageJobs {
		if job.UpdatedAt.Before(before) {
			delete(imageJobs, id)
		}
	}
}

func imagePromptFromContext(ctx context.Context, cfg serverConfig, req imageRequest) string {
	messages := requestMessages(chatRequest{Messages: req.Context})
	if len(messages) == 0 {
		return req.Prompt
	}
	prompts, _, err := readPromptSettings(cfg.promptsPath)
	if err != nil {
		log.Printf("image prompt context prompts failed: %v", err)
		prompts = defaultPromptSettings()
	}
	system := prompts.ImageContextRewriteSystem
	userPrompt := strings.ReplaceAll(prompts.ImageContextRewriteUser, "{{prompt}}", req.Prompt)
	messages = append(messages, ollamaMessage{
		Role:    "user",
		Content: userPrompt,
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

func releaseOllamaForImage(ctx context.Context, cfg serverConfig) {
	models := uniqueStrings(cfg.model, cfg.visionModel)
	for _, model := range models {
		if err := unloadOllamaModel(ctx, cfg.ollamaURL, model); err != nil {
			log.Printf("ollama unload %s before image generation failed: %v", model, err)
		}
	}
}

func waitForVoiceResourcesForImage(ctx context.Context, cfg serverConfig) error {
	baseURL := strings.TrimRight(cfg.voice.EutherLinkURL, "/")
	if baseURL == "" {
		return nil
	}

	deadline := time.Now().Add(45 * time.Minute)
	for attempt := 1; ; attempt++ {
		err := postResourceActionJSON(
			ctx,
			baseURL+"/v1/resources/heavy-tts/suspend",
			`{"seconds":900,"reason":"eutherpunk_sensenova_image_generation"}`,
		)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "409") {
			log.Printf("voice resource suspend before SenseNova image generation failed: %v", err)
			if stopErr := postResourceAction(ctx, baseURL+"/v1/resources/dots.tts/stop"); stopErr != nil {
				log.Printf("voice resource fallback dots stop before SenseNova image generation failed: %v", stopErr)
			}
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Dots TTS ar fortfarande upptagen efter 45 minuter; SenseNova-bildjobbet avbryts")
		}
		log.Printf("voice resource busy before SenseNova image generation; waiting for Dots TTS to finish (attempt %d)", attempt)
		if !sleepContext(ctx, 10*time.Second) {
			return ctx.Err()
		}
	}

	endpoints := []string{
		"/v1/resources/voxcpm2/unload",
	}
	for _, endpoint := range endpoints {
		if err := postResourceAction(ctx, baseURL+endpoint); err != nil {
			log.Printf("voice resource release %s before SenseNova image generation failed: %v", endpoint, err)
		}
	}
	return nil
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func postResourceAction(ctx context.Context, endpoint string) error {
	return postResourceActionWithBody(ctx, endpoint, "application/json", nil)
}

func postResourceActionJSON(ctx context.Context, endpoint, body string) error {
	return postResourceActionWithBody(ctx, endpoint, "application/json", strings.NewReader(body))
}

func postResourceActionWithBody(ctx context.Context, endpoint, contentType string, body io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resource action returned %s: %s", resp.Status, string(body))
	}
	return nil
}

func syncImageModelControl(ctx context.Context, image config.ImageConfig, model string) error {
	endpoint := strings.TrimSpace(image.ModelControlURL)
	if endpoint == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body := fmt.Sprintf("model = %q\n", normalizeImageModel(model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/toml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("image model control returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func unloadOllamaModel(ctx context.Context, ollamaURL, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	payload := map[string]any{
		"model":      model,
		"prompt":     "",
		"stream":     false,
		"keep_alive": 0,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ollamaURL, "/")+"/api/generate", bytes.NewReader(raw))
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
		return fmt.Errorf("ollama unload returned %s: %s", resp.Status, string(body))
	}
	return nil
}

func uniqueStrings(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
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

func askVisionOllama(ctx context.Context, cfg serverConfig, prompts promptSettings, system string, messages []ollamaMessage) (string, error) {
	answer, err := askOllama(ctx, cfg.ollamaURL, cfg.visionModel, system, messages)
	if err != nil {
		return "", err
	}
	answer = normalizeVisionAnswer(answer)
	if answer != "" {
		return answer, nil
	}
	for _, prompt := range []string{prompts.VisionFallbackBrief, prompts.VisionFallbackSubject} {
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

func askVisionOllamaDetailed(ctx context.Context, cfg serverConfig, prompts promptSettings, system string, messages []ollamaMessage) (string, string, error) {
	answer, err := askVisionOllama(ctx, cfg, prompts, system, messages)
	if err != nil {
		return "", "", err
	}
	metadata, err := askOllama(ctx, cfg.ollamaURL, cfg.visionModel, visionMetadataSystemPrompt(system, prompts), visionMetadataMessages(messages, prompts))
	if err != nil {
		return localizeVisionAnswer(ctx, cfg, prompts, answer), answer, nil
	}
	metadata = normalizeVisionMetadata(metadata)
	if metadata == "" {
		metadata = answer
	}
	answer = localizeVisionAnswer(ctx, cfg, prompts, answer)
	return answer, metadata, nil
}

func localizeVisionAnswer(ctx context.Context, cfg serverConfig, prompts promptSettings, answer string) string {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return ""
	}
	localized, err := askOllama(ctx, cfg.ollamaURL, cfg.model, prompts.VisionAnswerSystem, []ollamaMessage{{
		Role:    "user",
		Content: prompts.VisionAnswerUserPrefix + answer,
	}})
	if err != nil {
		return answer
	}
	localized = strings.TrimSpace(localized)
	if localized == "" {
		return answer
	}
	return localized
}

func visionMetadataSystemPrompt(base string, prompts promptSettings) string {
	return base + " " + prompts.VisionMetadataSystem
}

func visionMetadataMessages(messages []ollamaMessage, prompts promptSettings) []ollamaMessage {
	out := make([]ollamaMessage, 0, len(messages))
	for _, message := range messages {
		if len(message.Images) == 0 {
			continue
		}
		out = append(out, ollamaMessage{
			Role:    "user",
			Content: prompts.VisionMetadataUser,
			Images:  message.Images,
		})
	}
	return out
}

func normalizeVisionMetadata(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`")
	return strings.TrimSpace(value)
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
	if isEmptyVisionAnswer(answer) {
		return ""
	}
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

func isEmptyVisionAnswer(answer string) bool {
	normalized := strings.ToLower(answer)
	normalized = strings.Trim(normalized, " !?.:-_#*\"'")
	normalized = strings.Join(strings.Fields(normalized), " ")
	switch normalized {
	case "", "no animal", "no animals", "not an animal", "no animal shown", "there is no animal":
		return true
	}
	return false
}

func generateWithComfyUI(ctx context.Context, image config.ImageConfig, user string, req imageRequest) (imageResponse, error) {
	var out imageResponse
	baseURL := strings.TrimRight(image.ComfyUIURL, "/")
	if baseURL == "" {
		return out, errors.New("image.comfyui_url is not configured")
	}
	timeout := time.Duration(image.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if timeout < 15*time.Minute {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if isSenseNovaImageModel(req.ImageModel) && strings.TrimSpace(req.SourceImage) != "" {
		uploadedName, err := uploadComfySourceImage(ctx, baseURL, req.SourceImage)
		if err != nil {
			return out, err
		}
		req.SourceImage = uploadedName
	}

	prompt, err := buildImagePrompt(image, req)
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

func buildImagePrompt(image config.ImageConfig, req imageRequest) (map[string]any, error) {
	if isSenseNovaImageModel(req.ImageModel) {
		return buildSenseNovaPrompt(image, req)
	}
	return buildZImagePrompt(image, req)
}

func uploadComfySourceImage(ctx context.Context, baseURL, source string) (string, error) {
	data, contentType, err := decodeSourceImage(source)
	if err != nil {
		return "", err
	}
	ext := ".jpg"
	if strings.Contains(contentType, "png") {
		ext = ".png"
	} else if strings.Contains(contentType, "webp") {
		ext = ".webp"
	}
	filename := fmt.Sprintf("eutherpunk-source-%s%s", randomID(), ext)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.WriteField("type", "input"); err != nil {
		return "", err
	}
	if err := writer.WriteField("overwrite", "true"); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/upload/image", &body)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ComfyUI image upload returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var uploaded struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &uploaded); err != nil {
		return "", err
	}
	if uploaded.Name == "" {
		return "", fmt.Errorf("ComfyUI image upload missing name: %s", strings.TrimSpace(string(raw)))
	}
	return uploaded.Name, nil
}

func decodeSourceImage(source string) ([]byte, string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, "", errors.New("source_image is empty")
	}
	contentType := "image/jpeg"
	if strings.HasPrefix(source, "data:") {
		meta, payload, ok := strings.Cut(source, ",")
		if !ok {
			return nil, "", errors.New("source_image data URL saknar bilddata")
		}
		source = payload
		if strings.Contains(meta, ";base64") {
			contentType = strings.TrimPrefix(strings.Split(meta, ";")[0], "data:")
		} else {
			return nil, "", errors.New("source_image data URL maste vara base64")
		}
	}
	data, err := base64.StdEncoding.DecodeString(source)
	if err != nil {
		return nil, "", fmt.Errorf("source_image kunde inte base64-avkodas: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errors.New("source_image decoded to empty data")
	}
	return data, contentType, nil
}

func ensureSenseNovaReady(ctx context.Context, image config.ImageConfig, lora string) error {
	baseURL := strings.TrimRight(image.ComfyUIURL, "/")
	if baseURL == "" {
		return errors.New("image.comfyui_url is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/object_info/SenseNova_SM_Model", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("SenseNova ar inte redo: ComfyUI svarar inte: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("SenseNova ar inte redo: ComfyUI returned %s", resp.Status)
	}
	if !bytes.Contains(body, []byte(strconv.Quote(senseNovaGGUF))) {
		return fmt.Errorf("SenseNova laddas fortfarande: %s saknas i ComfyUI", senseNovaGGUF)
	}
	lora = normalizeLora(lora)
	if lora != "" && lora != "none" && !bytes.Contains(body, []byte(strconv.Quote(lora))) {
		return fmt.Errorf("SenseNova-LoRA saknas i ComfyUI: %s", lora)
	}
	return nil
}

func buildZImagePrompt(image config.ImageConfig, req imageRequest) (map[string]any, error) {
	width := clampToStep(defaultImageDimension(req.Width, image.DefaultWidth, safeImageDefaultWidth), 16, 1024, 16)
	height := clampToStep(defaultImageDimension(req.Height, image.DefaultHeight, safeImageDefaultHeight), 16, 1024, 16)
	steps := defaultImageSteps(req.Steps, image.DefaultSteps)
	if steps < 1 {
		steps = 1
	}
	if steps > 12 {
		steps = 12
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

func buildSenseNovaPrompt(image config.ImageConfig, req imageRequest) (map[string]any, error) {
	imageModel := normalizeImageModel(req.ImageModel)
	fastMode := imageModel == "sensenova-u1-8b-fast"
	steps := defaultImageSteps(req.Steps, image.DefaultSteps)
	if steps < 1 {
		steps = 1
	}
	if steps > 12 {
		steps = 12
	}
	seed := req.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	lora := normalizeLora(req.Lora)
	if lora == "" {
		lora = "none"
	}
	if fastMode {
		lora = "none"
	}
	targetPixels := "1:1"
	width := defaultImageDimension(req.Width, image.DefaultWidth, safeImageDefaultWidth)
	height := defaultImageDimension(req.Height, image.DefaultHeight, safeImageDefaultHeight)
	if width > height {
		targetPixels = "16:9"
	} else if height > width {
		targetPixels = "9:16"
	}
	imgMode := "edit"
	interleaveMax := 2
	if fastMode {
		imgMode = "interleave"
		interleaveMax = 1
	}
	samplerInputs := map[string]any{
		"model":              []any{"1", 0},
		"img_mode":           imgMode,
		"prompt":             req.Prompt,
		"seed":               int(seed % maxComfySeed),
		"steps":              steps,
		"target_pixels":      targetPixels,
		"cfg":                1.0,
		"img_cfg":            1.0,
		"timestep_shift":     3.0,
		"batch_size":         1,
		"prefetch_count":     1,
		"interleave_max":     interleaveMax,
		"cfg_norm":           "none",
		"enhance":            false,
		"think_mode":         false,
		"do_sample":          true,
		"max_new_tokens":     256,
		"temperature":        0.7,
		"top_p":              0.9,
		"top_k":              0,
		"repetition_penalty": 0.0,
	}
	workflow := map[string]any{
		"1": comfyNode("SenseNova_SM_Model", map[string]any{
			"diffusion_models": "none",
			"gguf":             senseNovaGGUF,
			"lora":             lora,
			"attn_backend":     "auto",
		}),
		"2": comfyNode("SenseNova_SM_Sampler", samplerInputs),
		"3": comfyNode("PreviewImage", map[string]any{
			"images": []any{"2", 0},
		}),
	}
	if strings.TrimSpace(req.SourceImage) != "" {
		workflow["4"] = comfyNode("LoadImage", map[string]any{
			"image": req.SourceImage,
		})
		samplerInputs["image"] = []any{"4", 0}
	}
	return workflow, nil
}

func defaultImageDimension(requested, configured, fallback int) int {
	if requested > 0 {
		return requested
	}
	if configured > 0 && configured < fallback {
		return configured
	}
	return fallback
}

func defaultImageSteps(requested, configured int) int {
	if requested > 0 {
		return requested
	}
	if configured > 0 && configured < safeImageDefaultSteps {
		return configured
	}
	return safeImageDefaultSteps
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

func defaultSettingsDirectory(image config.ImageConfig) string {
	imageDir := imageDirectory(image)
	base := filepath.Dir(imageDir)
	if base == "." || base == string(filepath.Separator) {
		base = "var"
	}
	return filepath.Join(base, "settings")
}

func defaultUserSettings(cfg serverConfig) userSettings {
	return userSettings{
		ChatModel:          cfg.model,
		VisionModel:        cfg.visionModel,
		ImageModel:         "z-image-turbo",
		ImageLora:          "none",
		VoiceBackend:       cfg.voice.ModelBackend,
		TTSEnabled:         false,
		ServerVoiceEnabled: false,
	}
}

func readUserSettings(cfg serverConfig, user string) (userSettings, error) {
	settings := defaultUserSettings(cfg)
	data, err := os.ReadFile(userSettingsPath(cfg.settingsDir, user))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return settings, nil
		}
		return settings, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(stripTOMLComment(line))
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value := mustTOMLString(strings.TrimSpace(raw))
		switch key {
		case "chat_model":
			settings.ChatModel = value
		case "vision_model":
			settings.VisionModel = value
		case "image_model":
			settings.ImageModel = normalizeImageModel(value)
		case "image_lora":
			settings.ImageLora = normalizeLora(value)
		case "voice_backend":
			settings.VoiceBackend = value
		case "tts_enabled":
			settings.TTSEnabled = mustTOMLBool(strings.TrimSpace(raw))
		case "server_voice_enabled":
			settings.ServerVoiceEnabled = mustTOMLBool(strings.TrimSpace(raw))
		}
	}
	finalizeUserSettings(&settings)
	return settings, nil
}

func defaultPromptsPath(settingsDir string) string {
	return filepath.Join(settingsDirectory(settingsDir), "prompts.toml")
}

func defaultPromptSettings() promptSettings {
	return promptSettings{
		DefaultSystem:             defaultSystemPrompt,
		VisionSystemSuffix:        "Nar anvandaren visar en bild, beskriv och resonera om bilden pa anvandarens sprak. Beskriv huvudmotiv, miljo, synliga objekt, text/markeringar och relevant osakerhet. Hitta inte pa saker du inte ser.",
		VisionFallbackBrief:       "Describe the image briefly in Swedish.",
		VisionFallbackSubject:     "What is the main subject of this image? Answer in Swedish.",
		VisionMetadataSystem:      "Du skriver dold bildmetadata for en annan lokal modell. Beskriv allt relevant i bilden detaljerat pa svenska: huvudmotiv, art/objekt, position, pose, blick, ansiktsuttryck, miljo, bakgrund, farger, ljus, stil, komposition, text i bilden, osakerheter och saker som kan vara viktiga for bildgenerering. Skriv inte som ett svar till anvandaren.",
		VisionMetadataUser:        "Skapa en riktigt detaljerad dold bildmetadata for bilden. Var konkret. Om bilden visar ett djur, ange trolig art om mojligt och namna osakerhet. Skriv pa svenska.",
		VisionAnswerSystem:        "Du skriver om bildtolkningar till kort, naturlig svenska. Behall sakuppgifter. Lagg inte till nya detaljer.",
		VisionAnswerUserPrefix:    "Skriv detta pa svenska, kort och konkret: ",
		ImageToolSystemSuffix:     "Om anvandaren vill skapa, generera, rita, kombinera eller variera en bild och du har tillracklig kontext, skriv en kort vanlig bekraftelse och lagg sedan en egen sista rad exakt i formatet: EUTHERPUNK_IMAGE_PROMPT: <en tydlig engelsk bildprompt>. Anvand sparad intern bildmetadata nar anvandaren refererar till tidigare bilder. Skriv aldrig denna rad for vanliga fragor.",
		ImageContextRewriteSystem: "You convert a chat conversation into one concise English prompt for an image generator. Use the latest user request as the instruction, include relevant visual context from earlier messages or images, and return only the final image prompt with no markdown or explanations.",
		ImageContextRewriteUser:   "Final image request: {{prompt}}\nWrite the image generation prompt.",
		HiddenImageMemoryTemplate: "Intern bildmetadata {{index}}: Detta ar EutherPunks sparade semantiska beskrivning av en tidigare bild i chatten, inte EXIF eller filmetadata. Om anvandaren fragar efter bildmetadata ska du visa eller sammanfatta denna text. Anvand den ocksa nar anvandaren refererar till bilden senare. Metadata: {{description}}",
	}
}

func readPromptSettings(path string) (promptSettings, string, error) {
	defaults := defaultPromptSettings()
	rawBytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw := formatPromptSettings(defaults)
		return defaults, raw, nil
	}
	if err != nil {
		return defaults, "", err
	}
	raw := string(rawBytes)
	prompts, err := parsePromptSettings(raw)
	if err != nil {
		return defaults, raw, err
	}
	return prompts, formatPromptSettings(prompts), nil
}

func parsePromptSettings(raw string) (promptSettings, error) {
	prompts := defaultPromptSettings()
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := stripTomlComment(scanner.Text())
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "prompts" {
			return prompts, fmt.Errorf("line %d: expected [prompts] section", lineNo)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return prompts, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		parsed, err := strconv.Unquote(strings.TrimSpace(value))
		if err != nil {
			return prompts, fmt.Errorf("line %d: %w", lineNo, err)
		}
		switch strings.TrimSpace(key) {
		case "default_system":
			prompts.DefaultSystem = parsed
		case "vision_system_suffix":
			prompts.VisionSystemSuffix = parsed
		case "vision_fallback_brief":
			prompts.VisionFallbackBrief = parsed
		case "vision_fallback_subject":
			prompts.VisionFallbackSubject = parsed
		case "vision_metadata_system":
			prompts.VisionMetadataSystem = parsed
		case "vision_metadata_user":
			prompts.VisionMetadataUser = parsed
		case "vision_answer_system":
			prompts.VisionAnswerSystem = parsed
		case "vision_answer_user_prefix":
			prompts.VisionAnswerUserPrefix = parsed
		case "image_tool_system_suffix":
			prompts.ImageToolSystemSuffix = parsed
		case "image_context_rewrite_system":
			prompts.ImageContextRewriteSystem = parsed
		case "image_context_rewrite_user":
			prompts.ImageContextRewriteUser = parsed
		case "hidden_image_memory_template":
			prompts.HiddenImageMemoryTemplate = parsed
		default:
			return prompts, fmt.Errorf("line %d: unknown prompt key %q", lineNo, strings.TrimSpace(key))
		}
	}
	return prompts, scanner.Err()
}

func formatPromptSettings(prompts promptSettings) string {
	var b strings.Builder
	b.WriteString("[prompts]\n")
	writePromptString(&b, "default_system", prompts.DefaultSystem)
	writePromptString(&b, "vision_system_suffix", prompts.VisionSystemSuffix)
	writePromptString(&b, "vision_fallback_brief", prompts.VisionFallbackBrief)
	writePromptString(&b, "vision_fallback_subject", prompts.VisionFallbackSubject)
	writePromptString(&b, "vision_metadata_system", prompts.VisionMetadataSystem)
	writePromptString(&b, "vision_metadata_user", prompts.VisionMetadataUser)
	writePromptString(&b, "vision_answer_system", prompts.VisionAnswerSystem)
	writePromptString(&b, "vision_answer_user_prefix", prompts.VisionAnswerUserPrefix)
	writePromptString(&b, "image_tool_system_suffix", prompts.ImageToolSystemSuffix)
	writePromptString(&b, "image_context_rewrite_system", prompts.ImageContextRewriteSystem)
	writePromptString(&b, "image_context_rewrite_user", prompts.ImageContextRewriteUser)
	writePromptString(&b, "hidden_image_memory_template", prompts.HiddenImageMemoryTemplate)
	return strings.TrimRight(b.String(), "\n")
}

func writePromptString(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(strconv.Quote(value))
	b.WriteByte('\n')
}

func stripTomlComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if r == '#' && !inString {
			return line[:i]
		}
	}
	return line
}

func writeUserSettings(root, user string, settings userSettings) error {
	dir := settingsDirectory(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := userSettingsPath(root, user)
	tmp := path + ".tmp"
	var b strings.Builder
	b.WriteString("# EutherPunk per-user settings\n")
	writeTOMLString(&b, "chat_model", settings.ChatModel)
	writeTOMLString(&b, "vision_model", settings.VisionModel)
	writeTOMLString(&b, "image_model", normalizeImageModel(settings.ImageModel))
	writeTOMLString(&b, "image_lora", normalizeLora(settings.ImageLora))
	writeTOMLString(&b, "voice_backend", settings.VoiceBackend)
	writeTOMLBool(&b, "tts_enabled", settings.TTSEnabled)
	writeTOMLBool(&b, "server_voice_enabled", settings.ServerVoiceEnabled)
	if err := os.WriteFile(tmp, []byte(b.String()), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mergeUserSettings(settings *userSettings, incoming userSettings) {
	if value := strings.TrimSpace(incoming.ChatModel); value != "" {
		settings.ChatModel = value
	}
	if value := strings.TrimSpace(incoming.VisionModel); value != "" {
		settings.VisionModel = value
	}
	if strings.TrimSpace(incoming.ImageModel) != "" {
		settings.ImageModel = normalizeImageModel(incoming.ImageModel)
	}
	if incoming.ImageLora != "" {
		settings.ImageLora = normalizeLora(incoming.ImageLora)
	}
	if value := strings.TrimSpace(incoming.VoiceBackend); value != "" {
		settings.VoiceBackend = value
	}
	settings.TTSEnabled = incoming.TTSEnabled
	settings.ServerVoiceEnabled = incoming.ServerVoiceEnabled
	finalizeUserSettings(settings)
}

func finalizeUserSettings(settings *userSettings) {
	if settings.ChatModel == "" {
		settings.ChatModel = "qwen3-coder:30b"
	}
	if settings.VisionModel == "" {
		settings.VisionModel = "moondream:latest"
	}
	settings.ImageModel = normalizeImageModel(settings.ImageModel)
	settings.ImageLora = normalizeLora(settings.ImageLora)
	if !isSenseNovaImageModel(settings.ImageModel) || settings.ImageModel == "sensenova-u1-8b-fast" {
		settings.ImageLora = "none"
	}
}

func normalizeImageModel(value string) string {
	switch strings.TrimSpace(value) {
	case "", "z-image", "z-image-turbo":
		return "z-image-turbo"
	case "sensenova", "sensenova-u1", "sensenova-u1-8b":
		return "sensenova-u1-8b"
	case "sensenova-fast", "sensenova-u1-fast", "sensenova-u1-8b-fast":
		return "sensenova-u1-8b-fast"
	default:
		return "z-image-turbo"
	}
}

func isSenseNovaImageModel(value string) bool {
	switch normalizeImageModel(value) {
	case "sensenova-u1-8b", "sensenova-u1-8b-fast":
		return true
	default:
		return false
	}
}

func normalizeLora(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "none" {
		return "none"
	}
	return filepath.Base(value)
}

func knownLoras(selected string) []string {
	out := []string{"none", senseNovaLoRA}
	selected = normalizeLora(selected)
	if selected != "none" && !stringInSlice(selected, out) {
		out = append(out, selected)
	}
	return out
}

func stringInSlice(value string, values []string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func settingsDirectory(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "var/settings"
	}
	if filepath.IsAbs(root) {
		return filepath.Clean(root)
	}
	return filepath.Clean(root)
}

func userSettingsPath(root, user string) string {
	return filepath.Join(settingsDirectory(root), safePathSegment(user)+".toml")
}

func stripTOMLComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case r == '#' && !inString:
			return line[:i]
		}
	}
	return line
}

func mustTOMLString(raw string) string {
	value, err := strconv.Unquote(raw)
	if err != nil {
		return strings.Trim(raw, `"`)
	}
	return strings.TrimSpace(value)
}

func mustTOMLBool(raw string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return value
}

func writeTOMLString(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(strconv.Quote(strings.TrimSpace(value)))
	b.WriteByte('\n')
}

func writeTOMLBool(b *strings.Builder, key string, value bool) {
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(strconv.FormatBool(value))
	b.WriteByte('\n')
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

func deleteConversation(root, user, id string) error {
	return os.Remove(conversationPath(root, user, id))
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
		image.Description = strings.TrimSpace(image.Description)
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

func systemPromptForMessages(prompts promptSettings, messages []ollamaMessage) string {
	base := prompts.DefaultSystem
	if messagesHaveImages(messages) {
		return base + " " + prompts.VisionSystemSuffix
	}
	return base
}

func systemPromptWithImageTool(base string, prompts promptSettings) string {
	suffix := strings.TrimSpace(prompts.ImageToolSystemSuffix)
	if suffix == "" {
		return base
	}
	return strings.TrimSpace(base) + " " + suffix
}

func chatModel(settings userSettings, messages []ollamaMessage) string {
	if settings.VisionModel != "" && messagesHaveImages(messages) {
		return settings.VisionModel
	}
	return settings.ChatModel
}

func selectedChatModel(settings userSettings, requested string, messages []ollamaMessage) string {
	if settings.VisionModel != "" && messagesHaveImages(messages) {
		return settings.VisionModel
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		return requested
	}
	return chatModel(settings, messages)
}

func isVisionRequest(settings userSettings, model string, messages []ollamaMessage) bool {
	return settings.VisionModel != "" && model == settings.VisionModel && messagesHaveImages(messages)
}

func messagesForSelectedModel(settings userSettings, model string, messages []ollamaMessage) []ollamaMessage {
	if model != settings.VisionModel || !messagesHaveImages(messages) {
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
			Content: "Svara pa svenska. Var kort och konkret. Beskriv vad bilden visar, inklusive huvudmotiv, miljo, synliga objekt och eventuell text eller markering. Om du ar osaker, sag vad som verkar troligt. Fraga: " + content,
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
