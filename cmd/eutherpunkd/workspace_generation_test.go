package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspacePlanMergesDuplicatePaths(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{
					"message":"repair engine",
					"files":[
						{"path":"engine.js","instruction":"Implement createGrid."},
						{"path":"./engine.js","instruction":"Implement nextGeneration."},
						{"path":"ENGINE.js","instruction":"Implement toggleCell."},
						{"path":"engine.js","instruction":"Implement createGrid."}
					]
				}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	plan, err := planWorkspaceOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "repair"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != "engine.js" {
		t.Fatalf("plan files = %#v", plan.Files)
	}
	for _, requirement := range []string{"createGrid", "nextGeneration", "toggleCell"} {
		if !strings.Contains(plan.Files[0].Instruction, requirement) {
			t.Fatalf("merged instruction %q misses %q", plan.Files[0].Instruction, requirement)
		}
	}
	if strings.Count(plan.Files[0].Instruction, "createGrid") != 1 {
		t.Fatalf("duplicate instruction was retained: %q", plan.Files[0].Instruction)
	}
}

func TestWorkspacePlanRejectsBinaryAssetFromTextChannel(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{
					"message":"skapar havet",
					"files":[{"path":"assets/ocean.png","instruction":"Returnera en PNG som Base64."}]
				}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	_, err := planWorkspaceOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "gör en bild av havet"}},
	)
	if err == nil || !strings.Contains(err.Error(), "binärfilen") {
		t.Fatalf("binary plan was not rejected: %v", err)
	}
}

func TestExistingWorkspaceFileUsesMinimalPatch(t *testing.T) {
	var calls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{
					Role: "assistant",
					Content: `{"message":"uppdaterar bakgrunden","files":[` +
						`{"path":"index.html","instruction":"Gör bakgrunden mörkare utan att ändra spelet."}]}`,
				},
				Done: true,
			})
			return
		}
		if request.Options["num_predict"] != float64(4096) {
			t.Fatalf("patch num_predict = %#v", request.Options["num_predict"])
		}
		if !workspaceRequestHasProperty(request, "edits") {
			t.Fatalf("existing file did not use edit schema: %#v", request.Format)
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role:    "assistant",
				Content: `{"edits":[{"old":"background: white;","new":"background: radial-gradient(circle, #222, #050509);"}]}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	messages := []ollamaMessage{{
		Role: "user",
		Content: "LOKAL KODARBETSYTA\n\n--- index.html ---\n" +
			"<style>body { background: white; }</style>\n" +
			"<script>const shadowPieces = true;</script>\n",
	}, {
		Role:    "user",
		Content: "gör en snyggare bakgrund",
	}}
	_, files, err := askWorkspaceOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		messages,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(files) != 1 {
		t.Fatalf("calls=%d files=%#v", calls, files)
	}
	if !strings.Contains(files[0].Content, "radial-gradient") ||
		!strings.Contains(files[0].Content, "shadowPieces = true") {
		t.Fatalf("patched file did not preserve behavior: %q", files[0].Content)
	}
}

func TestTruncatedCompleteFileRetriesAutomatically(t *testing.T) {
	var calls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if request.Options["num_predict"] != float64(12288) {
				t.Fatalf("first num_predict = %#v", request.Options["num_predict"])
			}
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message:    ollamaMessage{Role: "assistant", Content: `{"content":"unfinished`},
				Done:       true,
				DoneReason: "length",
				EvalCount:  12288,
			})
			return
		}
		if request.Options["num_predict"] != float64(16384) {
			t.Fatalf("retry num_predict = %#v", request.Options["num_predict"])
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role:    "assistant",
				Content: `{"content":"<!doctype html><title>Tetris</title>"}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	var progress []string
	content, err := generateCompleteWorkspaceFileOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "skapa tetris"}},
		workspacePlan{Files: []workspacePlanFile{{
			Path:        "index.html",
			Instruction: "Skapa ett komplett spel.",
		}}},
		workspacePlanFile{Path: "index.html", Instruction: "Skapa ett komplett spel."},
		func(message string) { progress = append(progress, message) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(content, "<title>Tetris</title>") {
		t.Fatalf("calls=%d content=%q", calls, content)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "utmatningsgränsen") {
		t.Fatalf("progress=%#v", progress)
	}
}

func TestWorkspaceFileEditsRejectAmbiguousMatch(t *testing.T) {
	_, err := applyWorkspaceFileEdits("same same", []workspaceFileEdit{{
		Old: "same",
		New: "changed",
	}})
	if err == nil || !strings.Contains(err.Error(), "2 gånger") {
		t.Fatalf("ambiguous edit error = %v", err)
	}
}
