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
