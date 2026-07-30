package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type workspacePlan struct {
	Message string              `json:"message"`
	Files   []workspacePlanFile `json:"files"`
}

type workspacePlanFile struct {
	Path        string `json:"path"`
	Instruction string `json:"instruction"`
}

// askWorkspaceOllama deliberately separates planning from code generation.
// The first, short call cannot return source code. Each following call returns
// one file through a structured channel, so source never becomes chat output.
func askWorkspaceOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	progress func(string),
) (string, []workspaceResponseFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()

	reportWorkspaceProgress(progress, "Modellen gör en kort filplan utan att skriva kod i chatten.")
	plan, err := planWorkspaceOllama(ctx, ollamaURL, model, system, messages)
	if err != nil {
		if strings.Contains(err.Error(), "tom kvalitetsgranskning") {
			return "Modellen gav ingen användbar filplan. Inga filer ändrades; försök igen.", nil, nil
		}
		return "", nil, err
	}
	if len(plan.Files) == 0 {
		return plan.Message, nil, nil
	}

	files := make([]workspaceResponseFile, 0, len(plan.Files))
	for index, planned := range plan.Files {
		reportWorkspaceProgress(
			progress,
			fmt.Sprintf("Skapar %s separat (%d/%d).", planned.Path, index+1, len(plan.Files)),
		)
		content, err := generateWorkspaceFileOllama(
			ctx, ollamaURL, model, system, messages, plan, planned, progress,
		)
		if err != nil {
			return "", nil, fmt.Errorf("skapa %s: %w", planned.Path, err)
		}
		files = append(files, workspaceResponseFile{
			Path:    planned.Path,
			Content: content,
		})
	}
	reportWorkspaceProgress(progress, "Alla filer är mottagna i den strukturerade filkanalen.")
	return plan.Message, files, nil
}

func planWorkspaceOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
) (workspacePlan, error) {
	plannerSystem := system + `

Du planerar ett lokalt kodjobb. Returnera endast en liten JSON-plan enligt
schemat. Skriv ingen källkod, inga kodblock och inget resonemang. "message" är
högst två korta meningar till användaren. Varje filpost anger en relativ path
och en precis instruction om filens ansvar, gränssnitt och acceptanskrav.
Välj minsta antal filer. När användaren ber att skapa eller ändra kod ska planen
innehålla filerna direkt; fråga inte om lov eftersom CLI:t gör det separat.
Varje relativ filsökväg får förekomma högst en gång. Samla alla ändringar för
samma fil i en enda filpost.

Filkanalen är endast för UTF-8-text. Planera aldrig PNG, JPEG, GIF, WebP,
ljud, video, typsnitt, PDF, arkiv eller andra binärfiler. När uppgiften
innehåller "HARNESS-VERIFIED IMMUTABLE ASSET" finns den filen redan lokalt:
planera endast de textbaserade källfiler som ska referera till dess exakta
sökväg.`
	format := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"message": map[string]any{"type": "string"},
			"files": map[string]any{
				"type":     "array",
				"maxItems": 16,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"path":        map[string]any{"type": "string"},
						"instruction": map[string]any{"type": "string"},
					},
					"required": []string{"path", "instruction"},
				},
			},
		},
		"required": []string{"message", "files"},
	}
	content, err := askWorkspaceStructuredTimeout(
		ctx, ollamaURL, model, plannerSystem, messages, format, 768, 90*time.Second,
	)
	if err != nil {
		return workspacePlan{}, err
	}
	var plan workspacePlan
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return workspacePlan{}, fmt.Errorf("tolka filplan: %w", err)
	}
	if len(plan.Files) > 16 {
		return workspacePlan{}, errors.New("filplanen innehåller för många filer")
	}
	normalized := make([]workspacePlanFile, 0, len(plan.Files))
	byPath := make(map[string]int, len(plan.Files))
	for i := range plan.Files {
		file := plan.Files[i]
		file.Path = normalizeWorkspacePlanPath(file.Path)
		file.Instruction = strings.TrimSpace(file.Instruction)
		if file.Path == "" || file.Path == "." || file.Instruction == "" {
			return workspacePlan{}, errors.New("filplanen innehåller en tom sökväg eller instruktion")
		}
		if isBinaryWorkspacePlanPath(file.Path) {
			return workspacePlan{}, fmt.Errorf(
				"filplanen försökte skapa binärfilen %q i textkanalen",
				file.Path,
			)
		}
		if len(file.Instruction) > 2000 {
			return workspacePlan{}, errors.New("filplanens instruktion är för lång")
		}
		key := strings.ToLower(file.Path)
		if index, exists := byPath[key]; exists {
			merged := mergeWorkspacePlanInstructions(
				normalized[index].Instruction,
				file.Instruction,
			)
			if len(merged) > 2000 {
				return workspacePlan{}, fmt.Errorf(
					"de sammanslagna instruktionerna för %q är för långa",
					file.Path,
				)
			}
			normalized[index].Instruction = merged
			continue
		}
		byPath[key] = len(normalized)
		normalized = append(normalized, file)
	}
	plan.Files = normalized
	plan.Message = compactWorkspaceMessage(plan.Message)
	return plan, nil
}

func isBinaryWorkspacePlanPath(value string) bool {
	switch strings.ToLower(filepath.Ext(value)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico",
		".woff", ".woff2", ".ttf", ".otf",
		".mp3", ".wav", ".ogg", ".flac",
		".mp4", ".webm", ".mov",
		".pdf", ".zip", ".gz", ".7z":
		return true
	default:
		return false
	}
}

func normalizeWorkspacePlanPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" {
		return ""
	}
	return path.Clean(value)
}

func mergeWorkspacePlanInstructions(existing, additional string) string {
	existing = strings.TrimSpace(existing)
	additional = strings.TrimSpace(additional)
	if additional == "" {
		return existing
	}
	if existing == "" {
		return additional
	}
	for _, instruction := range strings.Split(existing, "\n- ") {
		if strings.TrimSpace(instruction) == additional {
			return existing
		}
	}
	return existing + "\n- " + additional
}

func generateWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	plan workspacePlan,
	file workspacePlanFile,
	progress func(string),
) (string, error) {
	if existing, ok := workspaceSnapshotFile(messages, file.Path); ok {
		reportWorkspaceProgress(
			progress,
			fmt.Sprintf("Redigerar befintliga %s med en liten precisionspatch.", file.Path),
		)
		edited, err := editWorkspaceFileOllama(
			ctx, ollamaURL, model, system, messages, plan, file, existing,
		)
		if err == nil {
			return edited, nil
		}
		reportWorkspaceProgress(
			progress,
			fmt.Sprintf("Precisionspatchen för %s kunde inte tillämpas; faller tillbaka till en komplett fil.", file.Path),
		)
	}
	return generateCompleteWorkspaceFileOllama(
		ctx, ollamaURL, model, system, messages, plan, file, progress,
	)
}

type workspaceFileEdit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func editWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	plan workspacePlan,
	file workspacePlanFile,
	existing string,
) (string, error) {
	planJSON, err := json.Marshal(plan.Files)
	if err != nil {
		return "", err
	}
	editSystem := system + `

Du redigerar en befintlig fungerande fil med en liten strukturerad patch.
Returnera endast JSON enligt schemat med "edits". Varje "old" måste vara ett
ordagrant, sammanhängande och unikt stycke ur den befintliga filen. "new" är
ersättningen. Välj minsta möjliga stycken och bevara allt som uppgiften inte
uttryckligen ändrar. Returnera aldrig hela filen, Markdown, resonemang eller
ungefärliga matchningar.`
	editPrompt := fmt.Sprintf(
		"HELA FILPLANEN:\n%s\n\nREDIGERA ENDAST DEN BEFINTLIGA FILEN %q.\nKRAV:\n%s\n\nBEFINTLIG FIL:\n%s",
		planJSON,
		file.Path,
		file.Instruction,
		existing,
	)
	format := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"edits": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": 32,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"old": map[string]any{"type": "string"},
						"new": map[string]any{"type": "string"},
					},
					"required": []string{"old", "new"},
				},
			},
		},
		"required": []string{"edits"},
	}
	content, err := askWorkspaceStructuredTimeout(
		ctx, ollamaURL, model, editSystem,
		[]ollamaMessage{{Role: "user", Content: editPrompt}},
		format, 4096, 3*time.Minute,
	)
	if err != nil {
		return "", err
	}
	var patch struct {
		Edits []workspaceFileEdit `json:"edits"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return "", fmt.Errorf("tolka precisionspatch: %w", err)
	}
	return applyWorkspaceFileEdits(existing, patch.Edits)
}

func applyWorkspaceFileEdits(existing string, edits []workspaceFileEdit) (string, error) {
	if len(edits) == 0 || len(edits) > 32 {
		return "", errors.New("precisionspatchen innehåller fel antal redigeringar")
	}
	edited := existing
	for index, edit := range edits {
		if edit.Old == "" {
			return "", fmt.Errorf("precisionspatch %d har ett tomt sökstycke", index+1)
		}
		count := strings.Count(edited, edit.Old)
		if count != 1 {
			return "", fmt.Errorf(
				"precisionspatch %d matchar %d gånger i stället för exakt en",
				index+1,
				count,
			)
		}
		edited = strings.Replace(edited, edit.Old, edit.New, 1)
	}
	if edited == existing {
		return "", errors.New("precisionspatchen ändrade inte filen")
	}
	if strings.TrimSpace(edited) == "" {
		return "", errors.New("precisionspatchen tömde filen")
	}
	return edited, nil
}

func generateCompleteWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	plan workspacePlan,
	file workspacePlanFile,
	progress func(string),
) (string, error) {
	planJSON, err := json.Marshal(plan.Files)
	if err != nil {
		return "", err
	}
	fileSystem := system + `

Du producerar exakt en komplett fil genom en strukturerad filkanal. Returnera
endast JSON enligt schemat med fältet "content". Lägg aldrig koden i ett
chattsvar eller Markdown-kodstaket. Filen måste vara den enda slutliga,
sammanhängande versionen: inga TODO, placeholders, alternativa utkast eller
självrättningar. Kontrollera att alla anropade funktioner och identifierare
finns i filen eller målmiljöns standard-API. Om filen redan finns i
arbetsytesnapshoten ska den befintliga fungerande filen vara basen: bevara allt
som uppgiften inte uttryckligen ändrar och börja aldrig om med en ny design.`
	filePrompt := fmt.Sprintf(
		"HELA FILPLANEN:\n%s\n\nSKAPA ELLER UPPDATERA NU ENDAST FILEN %q.\nKRAV:\n%s",
		planJSON,
		file.Path,
		file.Instruction,
	)
	format := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"content"},
	}
	fileMessages := append([]ollamaMessage(nil), messages...)
	fileMessages = append(fileMessages, ollamaMessage{Role: "user", Content: filePrompt})
	content, err := askWorkspaceStructuredTimeout(
		ctx, ollamaURL, model, fileSystem, fileMessages, format, 12288, 5*time.Minute,
	)
	if err != nil {
		if errors.Is(err, errWorkspaceOutputTruncated) {
			reportWorkspaceProgress(progress, "Filsvaret nådde utmatningsgränsen; försöker en gång till med en större men mer kompakt fullfil.")
			return retryCompleteWorkspaceFileOllama(
				ctx, ollamaURL, model, fileSystem, fileMessages, format,
			)
		}
		return "", err
	}
	generated, parseErr := parseGeneratedWorkspaceFile(content)
	if parseErr == nil {
		return generated, nil
	}
	if errors.Is(parseErr, io.ErrUnexpectedEOF) {
		reportWorkspaceProgress(progress, "Filsvaret slutade mitt i JSON-formatet; försöker automatiskt en gång till.")
		return retryCompleteWorkspaceFileOllama(
			ctx, ollamaURL, model, fileSystem, fileMessages, format,
		)
	}
	return "", parseErr
}

func retryCompleteWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	format any,
) (string, error) {
	retryMessages := append([]ollamaMessage(nil), messages...)
	retryMessages = append(retryMessages, ollamaMessage{
		Role: "user",
		Content: "Det föregående filsvaret kapades. Försök igen från början. " +
			"Returnera samma kompletta fungerande fil, men håll implementationen kompakt och lämna inga funktioner halvfärdiga.",
	})
	content, err := askWorkspaceStructuredTimeout(
		ctx, ollamaURL, model, system, retryMessages, format, 16384, 5*time.Minute,
	)
	if err != nil {
		return "", err
	}
	return parseGeneratedWorkspaceFile(content)
}

func parseGeneratedWorkspaceFile(content string) (string, error) {
	var generated struct {
		Content string `json:"content"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return "", fmt.Errorf("tolka filinnehåll: %w", err)
	}
	if strings.TrimSpace(generated.Content) == "" {
		return "", errors.New("modellen returnerade en tom fil")
	}
	return generated.Content, nil
}

func workspaceSnapshotFile(messages []ollamaMessage, wantedPath string) (string, bool) {
	wantedPath = filepath.ToSlash(strings.TrimSpace(wantedPath))
	if wantedPath == "" {
		return "", false
	}
	header := "\n--- " + wantedPath + " ---\n"
	for _, message := range messages {
		source := "\n" + strings.ReplaceAll(message.Content, "\r\n", "\n")
		start := strings.Index(source, header)
		if start < 0 {
			continue
		}
		start += len(header)
		end := strings.Index(source[start:], "\n--- ")
		if end < 0 {
			return source[start:], true
		}
		return source[start : start+end], true
	}
	return "", false
}

func compactWorkspaceMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		message = strings.TrimSpace(message[:500])
	}
	if strings.Contains(message, "```") {
		return "Filförslaget är skapat i den separata filkanalen."
	}
	return message
}
