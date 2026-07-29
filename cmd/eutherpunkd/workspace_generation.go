package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			ctx, ollamaURL, model, system, messages, plan, planned,
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
innehålla filerna direkt; fråga inte om lov eftersom CLI:t gör det separat.`
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
	for i := range plan.Files {
		plan.Files[i].Path = strings.TrimSpace(plan.Files[i].Path)
		plan.Files[i].Instruction = strings.TrimSpace(plan.Files[i].Instruction)
		if plan.Files[i].Path == "" || plan.Files[i].Instruction == "" {
			return workspacePlan{}, errors.New("filplanen innehåller en tom sökväg eller instruktion")
		}
		if len(plan.Files[i].Instruction) > 2000 {
			return workspacePlan{}, errors.New("filplanens instruktion är för lång")
		}
	}
	plan.Message = compactWorkspaceMessage(plan.Message)
	return plan, nil
}

func generateWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	plan workspacePlan,
	file workspacePlanFile,
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
finns i filen eller målmiljöns standard-API.`
	filePrompt := fmt.Sprintf(
		"HELA FILPLANEN:\n%s\n\nSKAPA NU ENDAST FILEN %q.\nKRAV:\n%s",
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
		ctx, ollamaURL, model, fileSystem, fileMessages, format, 6144, 5*time.Minute,
	)
	if err != nil {
		return "", err
	}
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
