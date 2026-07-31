package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	assetRegistryFileName = "assets.json"
	assetRegistryVersion  = 1
	maxAssetRegistrySize  = 64 * 1024
	maxAssetRegistryItems = 64
)

type workspaceAssetIntent struct {
	Role          string
	LogicalName   string
	ImagePrompt   string
	Original      string
	PreviousAsset *workspaceAssetRecord
	UseExisting   bool
	DisplayOnly   bool
}

type workspaceAssetRegistry struct {
	Version int                    `json:"version"`
	Assets  []workspaceAssetRecord `json:"assets"`
}

type workspaceAssetRecord struct {
	ID          string `json:"id"`
	LogicalName string `json:"logical_name"`
	Role        string `json:"role"`
	Path        string `json:"path"`
	Prompt      string `json:"prompt"`
	Request     string `json:"request"`
	SHA256      string `json:"sha256"`
	Status      string `json:"status"`
	Supersedes  string `json:"supersedes,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type preparedWorkspaceAsset struct {
	Path       string
	CodePrompt string
	Summary    string
	Completed  bool
}

var nonAssetSlugCharacters = regexp.MustCompile(`[^a-z0-9]+`)

func prepareNaturalWorkspaceAsset(
	cfg cliConfig,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	request string,
	history []chatMessage,
	output io.Writer,
) (preparedWorkspaceAsset, bool, error) {
	registry, err := loadWorkspaceAssetRegistry(cfg.workspace)
	if err != nil {
		return preparedWorkspaceAsset{}, false, err
	}
	intent, ok := detectWorkspaceAssetIntent(request, registry)
	if !ok {
		return preparedWorkspaceAsset{}, false, nil
	}
	if err := ensureProjectMemory(cfg.workspace, request); err != nil {
		return preparedWorkspaceAsset{}, true, err
	}
	var record workspaceAssetRecord
	if intent.UseExisting {
		if intent.PreviousAsset == nil {
			return preparedWorkspaceAsset{}, true, errors.New("ingen befintlig asset valdes")
		}
		record = *intent.PreviousAsset
		if err := verifyRegisteredWorkspaceAsset(cfg.workspace, record); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
		if err := recordAssetWorkflowState(
			cfg.workspace,
			"asset_integrating",
			record.LogicalName,
			record.Path,
			request,
			"Using an existing verified asset for deterministic integration.",
		); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
	} else {
		if err := recordAssetWorkflowState(
			cfg.workspace,
			"asset_pending",
			intent.LogicalName,
			"",
			request,
			"Generating project image asset before code integration.",
		); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}

		preferred := semanticAssetFilename(intent.LogicalName)
		asset, err := createCLIImageAsset(
			cfg,
			reader,
			permissions,
			intent.ImagePrompt,
			history,
			output,
			preferred,
		)
		if err != nil {
			_ = recordAssetWorkflowState(
				cfg.workspace,
				"asset_failed",
				intent.LogicalName,
				"",
				request,
				err.Error(),
			)
			return preparedWorkspaceAsset{}, true, err
		}
		if !asset.Saved {
			_ = recordAssetWorkflowState(
				cfg.workspace,
				"asset_cancelled",
				intent.LogicalName,
				"",
				request,
				"Image generated but local asset write was not approved.",
			)
			return preparedWorkspaceAsset{}, true, nil
		}

		record, err = registerWorkspaceAsset(
			cfg.workspace,
			registry,
			intent,
			asset.Path,
		)
		if err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
		if err := recordAssetWorkflowState(
			cfg.workspace,
			"asset_ready",
			record.LogicalName,
			record.Path,
			request,
			"Generated asset is ready for code integration.",
		); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
	}
	if intent.DisplayOnly {
		summary := "Bildasset skapad och visad: " + record.Path
		if err := recordAssetWorkflowState(
			cfg.workspace,
			"asset_displayed",
			record.LogicalName,
			record.Path,
			request,
			summary,
		); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
		return preparedWorkspaceAsset{
			Path:      record.Path,
			Summary:   summary,
			Completed: true,
		}, true, nil
	}

	supported, applied, err := integrateWorkspaceAssetDeterministically(
		cfg.workspace,
		reader,
		permissions,
		record,
	)
	if err != nil {
		_ = recordAssetWorkflowState(
			cfg.workspace,
			"asset_integration_failed",
			record.LogicalName,
			record.Path,
			request,
			err.Error(),
		)
		return preparedWorkspaceAsset{}, true, err
	}
	if supported {
		status := "asset_integration_skipped"
		summary := "Bildasseten är sparad men index.html ändrades inte."
		if applied {
			status = "asset_integrated"
			summary = "Bildasseten kopplades in direkt i index.html utan ett kodmodelljobb."
		}
		if err := recordAssetWorkflowState(
			cfg.workspace,
			status,
			record.LogicalName,
			record.Path,
			request,
			summary,
		); err != nil {
			return preparedWorkspaceAsset{}, true, err
		}
		return preparedWorkspaceAsset{
			Path:      record.Path,
			Summary:   summary,
			Completed: true,
		}, true, nil
	}

	codePrompt := fmt.Sprintf(
		"USER GOAL (MEDIA PHASE ALREADY COMPLETED):\n%s\n\n"+
			"HARNESS-VERIFIED IMMUTABLE ASSET:\n"+
			"The CLI image tool has already generated, validated and saved a real PNG at `%s` "+
			"inside the selected workspace. SHA-256: `%s`.\n"+
			"Treat that file's existence as established during planning and quality review. "+
			"It is binary input outside the model's text-only file channel and MUST NOT appear "+
			"in the file plan or proposal. Never regenerate, rename, encode or replace it.\n\n"+
			"CODE PHASE:\nUpdate the existing project to reference exactly that relative path "+
			"as the requested %s. Propose only the smallest necessary textual source-file changes. "+
			"Preserve all unrelated working behavior, gameplay, controls and layout.",
		request,
		record.Path,
		record.SHA256,
		record.Role,
	)
	if intent.PreviousAsset != nil {
		codePrompt += fmt.Sprintf(
			"\nReplace references to the previous active asset `%s` where appropriate.",
			intent.PreviousAsset.Path,
		)
	}
	return preparedWorkspaceAsset{
		Path:       record.Path,
		CodePrompt: codePrompt,
		Summary:    "Bildasset skapad och överlämnad till kodaren: " + record.Path,
	}, true, nil
}

func detectWorkspaceAssetIntent(
	request string,
	registry workspaceAssetRegistry,
) (workspaceAssetIntent, bool) {
	normalized := normalizeAssetIntentText(request)
	if normalized == "" {
		return workspaceAssetIntent{}, false
	}
	previous := referencedWorkspaceAsset(normalized, registry)
	role := detectAssetRole(normalized)
	integrationRequest := containsAnyPhrase(normalized,
		"koppla in", "lägg in", "lagg in", "använd", "anvand",
		"use as", "use it", "set as",
	)
	if previous == nil && role != "" && integrationRequest {
		previous = latestActiveWorkspaceAsset(registry, role)
	}
	if role == "" && previous != nil {
		role = previous.Role
	}
	explicitImage := containsAnyPhrase(normalized,
		"bildgenererad", "bild genererad", "genererad bild", "riktig bild",
		"image generated", "generated image", "generate image", "create image",
		"skapa en bild", "generera en bild", "gör en bild", "göra en bild",
		"rita en bild", "bild av", "bild på", "image of", "picture of",
		"bildasset", "image asset",
	)
	standaloneImage := containsAnyPhrase(normalized,
		"skapa en bild på", "skapa en bild av",
		"generera en bild på", "generera en bild av",
		"gör en bild på", "gör en bild av",
		"göra en bild på", "göra en bild av",
		"rita en bild på", "rita en bild av",
		"create an image of", "create a picture of",
		"generate an image of", "generate a picture of",
		"make an image of", "make a picture of",
	)
	assetRequest := containsAnyPhrase(normalized,
		"asset", "assett", "lägg in", "lagg in", "använd", "anvand", "use it",
	)
	if role == "" && explicitImage && (standaloneImage || assetRequest) {
		role = "asset"
	}
	visualRevision := previous != nil && containsAnyPhrase(normalized,
		"morkare", "ljusare", "mer realist", "annan stil", "andra stil",
		"byt", "andra", "forandra", "modify", "darker", "lighter",
		"more realistic", "regenerate", "replace",
	)
	useExisting := previous != nil && integrationRequest && !visualRevision && !explicitImage
	if role == "" || (!explicitImage && !visualRevision && !useExisting) {
		return workspaceAssetIntent{}, false
	}
	logicalName := logicalAssetName(normalized, role, previous)
	imagePrompt := "Create a polished standalone project image asset with no text, logo, watermark or user interface. " +
		"Original request: " + strings.TrimSpace(request)
	if previous != nil {
		imagePrompt = "Create a revised replacement for a previous project image asset. " +
			"Previous generation prompt: " + previous.Prompt + ". New request: " +
			strings.TrimSpace(request) + ". Return the image only with no text, logo, watermark or user interface."
	}
	return workspaceAssetIntent{
		Role:          role,
		LogicalName:   logicalName,
		ImagePrompt:   imagePrompt,
		Original:      strings.TrimSpace(request),
		PreviousAsset: previous,
		UseExisting:   useExisting,
		DisplayOnly:   standaloneImage && !integrationRequest && !assetRequest,
	}, true
}

func latestActiveWorkspaceAsset(
	registry workspaceAssetRegistry,
	role string,
) *workspaceAssetRecord {
	for index := len(registry.Assets) - 1; index >= 0; index-- {
		asset := registry.Assets[index]
		if asset.Status == "active" && asset.Role == role {
			return &asset
		}
	}
	return nil
}

func detectAssetRole(normalized string) string {
	switch {
	case containsAnyPhrase(normalized, "bakgrund", "background", "backdrop"):
		return "background"
	case containsAnyPhrase(normalized, "sprite", "figur", "karaktar", "character"):
		return "sprite"
	case containsAnyPhrase(normalized, "textur", "texture", "tile", "tileset"):
		return "texture"
	case containsAnyPhrase(normalized, "ikon", "icon"):
		return "icon"
	case containsAnyPhrase(normalized, "omslag", "cover"):
		return "cover"
	default:
		return ""
	}
}

func logicalAssetName(
	normalized, role string,
	previous *workspaceAssetRecord,
) string {
	if previous != nil && previous.LogicalName != "" {
		return previous.LogicalName
	}
	subject := ""
	switch {
	case containsAnyPhrase(normalized, "hav", "havet", "ocean", "sea"):
		subject = "ocean"
	case containsAnyPhrase(normalized, "skog", "forest"):
		subject = "forest"
	case containsAnyPhrase(normalized, "rymd", "space", "galaxy"):
		subject = "space"
	case containsAnyPhrase(normalized, "stad", "city"):
		subject = "city"
	case containsAnyPhrase(normalized, "berg", "mountain"):
		subject = "mountain"
	}
	if subject == "" {
		subject = "generated"
	}
	return safeAssetSlug(subject + "-" + role)
}

func referencedWorkspaceAsset(
	normalized string,
	registry workspaceAssetRegistry,
) *workspaceAssetRecord {
	requestWords := assetIntentWords(normalized)
	for index := len(registry.Assets) - 1; index >= 0; index-- {
		asset := &registry.Assets[index]
		if asset.Status != "active" {
			continue
		}
		if strings.Contains(normalized, strings.ReplaceAll(asset.LogicalName, "-", " ")) {
			return asset
		}
		sourceWords := assetIntentWords(normalizeAssetIntentText(asset.Request + " " + asset.LogicalName))
		for word := range requestWords {
			if len(word) >= 4 && sourceWords[word] {
				copy := *asset
				return &copy
			}
		}
	}
	return nil
}

func loadWorkspaceAssetRegistry(workspace workspaceState) (workspaceAssetRegistry, error) {
	registry := workspaceAssetRegistry{Version: assetRegistryVersion}
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return registry, err
	}
	path := filepath.Join(dir, assetRegistryFileName)
	exists, err := safeProjectMemoryFileExists(path)
	if err != nil {
		return registry, err
	}
	if !exists {
		return registry, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return registry, err
	}
	if len(raw) > maxAssetRegistrySize {
		return registry, errors.New("projektets assetregister är för stort")
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return registry, err
	}
	if registry.Version != assetRegistryVersion {
		return registry, fmt.Errorf("assetregisterversion %d stöds inte", registry.Version)
	}
	return registry, nil
}

func registerWorkspaceAsset(
	workspace workspaceState,
	registry workspaceAssetRegistry,
	intent workspaceAssetIntent,
	path string,
) (workspaceAssetRecord, error) {
	root, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return workspaceAssetRecord{}, err
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return workspaceAssetRecord{}, err
	}
	sum := sha256.Sum256(raw)
	record := workspaceAssetRecord{
		ID:          fmt.Sprintf("asset-%d", time.Now().UTC().UnixNano()),
		LogicalName: intent.LogicalName,
		Role:        intent.Role,
		Path:        filepath.ToSlash(path),
		Prompt:      intent.ImagePrompt,
		Request:     intent.Original,
		SHA256:      hex.EncodeToString(sum[:]),
		Status:      "active",
		CreatedAt:   projectMemoryTimestamp(),
	}
	if intent.PreviousAsset != nil {
		record.Supersedes = intent.PreviousAsset.ID
	}
	for index := range registry.Assets {
		if registry.Assets[index].Status == "active" &&
			registry.Assets[index].LogicalName == record.LogicalName {
			registry.Assets[index].Status = "superseded"
		}
	}
	registry.Version = assetRegistryVersion
	registry.Assets = append(registry.Assets, record)
	if len(registry.Assets) > maxAssetRegistryItems {
		registry.Assets = registry.Assets[len(registry.Assets)-maxAssetRegistryItems:]
	}
	encoded, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return workspaceAssetRecord{}, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxAssetRegistrySize {
		return workspaceAssetRecord{}, errors.New("projektets assetregister blev för stort")
	}
	if err := writeProjectMemoryFileAtomic(filepath.Join(dir, assetRegistryFileName), encoded); err != nil {
		return workspaceAssetRecord{}, err
	}
	return record, nil
}

func recordAssetWorkflowState(
	workspace workspaceState,
	status, name, path, request, message string,
) error {
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return err
	}
	state, err := loadProjectMemoryState(dir)
	if err != nil {
		return err
	}
	state.Status = status
	state.AssetStatus = status
	state.AssetName = compactProjectText(name, 200)
	state.AssetPath = compactProjectText(path, 500)
	state.AssetRequest = compactProjectText(request, 1000)
	state.LastTask = state.AssetRequest
	state.Model = ""
	state.JobID = ""
	state.CheckpointRevision = 0
	state.Issues = nil
	state.Files = assetWorkflowFiles(status, path)
	state.Summary = compactProjectText(message, 2000)
	state.UpdatedAt = projectMemoryTimestamp()
	if err := saveProjectMemoryState(dir, state); err != nil {
		return err
	}
	return appendProjectJournal(dir, projectJournalEvent{
		Type:    "asset_workflow",
		Status:  status,
		Task:    state.AssetRequest,
		Files:   state.Files,
		Message: state.Summary,
	})
}

func semanticAssetFilename(logicalName string) string {
	return fmt.Sprintf(
		"%s-%s.png",
		safeAssetSlug(logicalName),
		time.Now().UTC().Format("20060102-150405.000000000"),
	)
}

func safeAssetSlug(value string) string {
	value = nonAssetSlugCharacters.ReplaceAllString(strings.ToLower(value), "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "generated-asset"
	}
	if len(value) > 64 {
		value = strings.Trim(value[:64], "-")
	}
	return value
}

func normalizeAssetIntentText(value string) string {
	value = strings.NewReplacer(
		"å", "a", "ä", "a", "ö", "o",
		"Å", "a", "Ä", "a", "Ö", "o",
		"é", "e", "è", "e", "ê", "e",
		"É", "e", "È", "e", "Ê", "e",
	).Replace(value)
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
		} else {
			builder.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

func containsAnyPhrase(value string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(value, normalizeAssetIntentText(phrase)) {
			return true
		}
	}
	return false
}

func assetIntentWords(value string) map[string]bool {
	stop := map[string]bool{
		"skulle": true, "vilja": true, "fint": true, "riktig": true,
		"bild": true, "asset": true, "background": true, "bakgrund": true,
		"with": true, "have": true, "that": true, "this": true,
		"som": true, "med": true, "och": true, "den": true, "det": true,
	}
	words := make(map[string]bool)
	for _, word := range strings.Fields(value) {
		if !stop[word] {
			words[word] = true
		}
	}
	return words
}

func compactAssetPath(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return []string{filepath.ToSlash(path)}
}

func assetWorkflowFiles(status, path string) []string {
	files := compactAssetPath(path)
	if status == "asset_integrated" {
		files = append(files, "index.html")
	}
	return files
}
