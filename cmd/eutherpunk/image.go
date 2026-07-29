package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const imageToolPrefix = "EUTHERPUNK_IMAGE_PROMPT:"
const maxCLIImageAssetBytes = 32 * 1024 * 1024

var cliImagePollInterval = time.Second

type cliImageRequest struct {
	Prompt  string        `json:"prompt"`
	Context []chatMessage `json:"context,omitempty"`
}

type cliImageResponse struct {
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url"`
}

type cliImageJob struct {
	ID       string           `json:"job_id"`
	Status   string           `json:"status"`
	Message  string           `json:"message,omitempty"`
	Response cliImageResponse `json:"image,omitempty"`
	Error    string           `json:"error,omitempty"`
}

type imageDirectiveWriter struct {
	mu      sync.Mutex
	output  io.Writer
	pending string
	err     error
}

func newImageDirectiveWriter(output io.Writer) *imageDirectiveWriter {
	return &imageDirectiveWriter{output: output}
}

func (w *imageDirectiveWriter) Write(raw []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.pending += string(raw)
	for {
		index := strings.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		line := w.pending[:index]
		w.pending = w.pending[index+1:]
		if _, _, ok := extractImageToolDirective(line); ok {
			continue
		}
		if _, err := io.WriteString(w.output, line+"\n"); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(raw), nil
}

func (w *imageDirectiveWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.pending == "" {
		return nil
	}
	line := w.pending
	w.pending = ""
	if _, _, ok := extractImageToolDirective(line); ok {
		return nil
	}
	_, w.err = io.WriteString(w.output, line)
	return w.err
}

func extractImageToolDirective(text string) (visible, prompt string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	visibleLines := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= len(imageToolPrefix) &&
			strings.EqualFold(trimmed[:len(imageToolPrefix)], imageToolPrefix) {
			candidate := strings.TrimSpace(trimmed[len(imageToolPrefix):])
			if candidate != "" {
				prompt = candidate
				ok = true
				continue
			}
		}
		visibleLines = append(visibleLines, line)
	}
	return strings.TrimSpace(strings.Join(visibleLines, "\n")), prompt, ok
}

func generateCLIImage(
	ctx context.Context,
	cfg cliConfig,
	prompt string,
	history []chatMessage,
	output io.Writer,
) (cliImageResponse, error) {
	var image cliImageResponse
	raw, err := json.Marshal(cliImageRequest{
		Prompt:  strings.TrimSpace(prompt),
		Context: history,
	})
	if err != nil {
		return image, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		cfg.apiURL+"/api/eutherpunk/images/generate",
		bytes.NewReader(raw),
	)
	if err != nil {
		return image, err
	}
	setCLIRequestHeaders(req)
	if err := cfg.authorize(req); err != nil {
		return image, err
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return image, err
	}
	defer resp.Body.Close()
	var job cliImageJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return image, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return image, imageJobHTTPError(resp.Status, job.Error)
	}
	if job.ID == "" {
		if job.Response.URL == "" {
			return image, errors.New("bildservern returnerade varken jobb-ID eller bild-URL")
		}
		return job.Response, nil
	}

	ticker := time.NewTicker(cliImagePollInterval)
	defer ticker.Stop()
	lastMessage := ""
	for {
		select {
		case <-ctx.Done():
			return image, ctx.Err()
		case <-ticker.C:
			current, err := fetchCLIImageJob(ctx, cfg, job.ID)
			if err != nil {
				return image, err
			}
			if current.Message != "" && current.Message != lastMessage {
				fmt.Fprintln(output, "eutherpunk>", current.Message)
				lastMessage = current.Message
			}
			switch strings.ToLower(strings.TrimSpace(current.Status)) {
			case "done":
				if current.Response.URL == "" {
					return image, errors.New("bildjobbet blev klart utan bild-URL")
				}
				return current.Response, nil
			case "error", "failed":
				if current.Error == "" {
					current.Error = "bildjobbet misslyckades"
				}
				return image, errors.New(current.Error)
			}
		}
	}
}

func runCLIImageAsset(
	cfg cliConfig,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	prompt string,
	history []chatMessage,
	output io.Writer,
) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("använd /image <bildprompt>")
	}
	fmt.Fprintln(output, "eutherpunk> genererar bildasset… (Esc Esc avbryter)")
	var image cliImageResponse
	_, err := runInterruptibleAgentCall(output, func(ctx context.Context) (string, error) {
		var generateErr error
		image, generateErr = generateCLIImage(ctx, cfg, prompt, history, output)
		return "", generateErr
	})
	if err != nil {
		if errors.Is(err, errAgentInterrupted) {
			return "", errors.New("bildjobbet avbröts")
		}
		if strings.Contains(err.Error(), "insufficient_scope") {
			return "", errors.New("din befintliga CLI-inloggning saknar mediaåtkomst; kör /logout och därefter /auth login för att godkänna det nya scopet")
		}
		return "", err
	}
	assetPath, saved, err := saveCLIImageAsset(
		context.Background(),
		cfg,
		reader,
		permissions,
		image,
	)
	if err != nil {
		return "", err
	}
	if saved {
		fmt.Fprintln(output, "eutherpunk> bildasset sparad:", assetPath)
		return "Bildasset sparad i arbetsytan: " + assetPath, nil
	}
	imageURL := absoluteCLIURL(cfg.apiURL, image.URL)
	fmt.Fprintln(output, "eutherpunk> bild klar på servern:", imageURL)
	return "Bild klar på servern: " + imageURL, nil
}

func fetchCLIImageJob(ctx context.Context, cfg cliConfig, id string) (cliImageJob, error) {
	var job cliImageJob
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		cfg.apiURL+"/api/eutherpunk/images/jobs/"+url.PathEscape(id),
		nil,
	)
	if err != nil {
		return job, err
	}
	setCLIRequestHeaders(req)
	if err := cfg.authorize(req); err != nil {
		return job, err
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return job, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return job, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return job, imageJobHTTPError(resp.Status, job.Error)
	}
	return job, nil
}

func saveCLIImageAsset(
	ctx context.Context,
	cfg cliConfig,
	reader interface{ ReadString(byte) (string, error) },
	permissions *sessionPermissions,
	image cliImageResponse,
) (string, bool, error) {
	if cfg.workspace.Root == "" {
		return "", false, nil
	}
	if permissions.files == permissionOff {
		fmt.Println("Bildasseten sparades inte lokalt eftersom filåtkomst är OFF.")
		return "", false, nil
	}
	filename := safeCLIImageFilename(image.Filename)
	relative := filepath.ToSlash(filepath.Join("assets", filename))
	if permissions.files != permissionAuto {
		fmt.Printf("Spara bildasseten som %s? [y/N]: ", relative)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return "", false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes", "j", "ja":
		default:
			fmt.Println("Bildasseten sparades inte lokalt.")
			return "", false, nil
		}
	}
	raw, err := downloadCLIImage(ctx, cfg, image.URL)
	if err != nil {
		return "", false, err
	}
	root, err := canonicalWorkspaceRoot(cfg.workspace)
	if err != nil {
		return "", false, err
	}
	if err := writeWorkspaceBytes(root, relative, raw); err != nil {
		return "", false, err
	}
	return relative, true, nil
}

func downloadCLIImage(ctx context.Context, cfg cliConfig, imageURL string) ([]byte, error) {
	absolute, err := authorizedCLIURL(cfg.apiURL, imageURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, absolute, nil)
	if err != nil {
		return nil, err
	}
	setCLIRequestHeaders(req)
	if err := cfg.authorize(req); err != nil {
		return nil, err
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bildhämtning: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCLIImageAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCLIImageAssetBytes {
		return nil, errors.New("bildasseten är större än 32 MiB")
	}
	if len(raw) < 8 || !bytes.Equal(raw[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("bildservern returnerade inte en giltig PNG")
	}
	return raw, nil
}

func writeWorkspaceBytes(root, relative string, raw []byte) error {
	target, exists, err := safeWorkspaceTarget(root, relative)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("bildasseten finns redan: %s", relative)
	}
	parent := filepath.Dir(target)
	if err := mkdirWorkspaceParents(root, parent); err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, ".eutherpunk-*.new")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFile(tempPath, target)
}

func safeCLIImageFilename(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	if value == "." || value == "" || !strings.EqualFold(filepath.Ext(value), ".png") {
		value = fmt.Sprintf("eutherpunk-%s.png", time.Now().UTC().Format("20060102-150405.000000000"))
	}
	return value
}

func setCLIRequestHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
}

func imageJobHTTPError(status, detail string) error {
	if detail != "" {
		return fmt.Errorf("%s: %s", status, detail)
	}
	return errors.New(status)
}

func authorizedCLIURL(baseURL, value string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	if err != nil {
		return "", err
	}
	target, err := base.Parse(value)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(base.Scheme, target.Scheme) ||
		!strings.EqualFold(base.Host, target.Host) {
		return "", errors.New("bild-URL lämnar EutherPunk-servern")
	}
	return target.String(), nil
}

func absoluteCLIURL(baseURL, value string) string {
	resolved, err := authorizedCLIURL(baseURL, value)
	if err != nil {
		return value
	}
	return resolved
}
