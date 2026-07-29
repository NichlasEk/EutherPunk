package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDatasetExportsVerifiedRepairAndDeduplicates(t *testing.T) {
	inputRoot := t.TempDir()
	trace := datasetTestTrace("return 0", "return 42")
	writeTestJSON(t, filepath.Join(inputRoot, "first.json"), trace)
	writeTestJSON(t, filepath.Join(inputRoot, "duplicate.json"), trace)
	writeTestJSON(t, filepath.Join(inputRoot, "summary.json"), evalRunResult{
		SchemaVersion: evalSchemaVersion,
		SuiteName:     "not-a-trace",
	})
	outputRoot := filepath.Join(t.TempDir(), "dataset")
	manifest, err := buildDataset(datasetOptions{
		Inputs:         []string{inputRoot},
		Output:         outputRoot,
		HoldoutPercent: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TraceFilesInspected != 3 ||
		manifest.AcceptedTraces != 2 ||
		manifest.RepairTransitions != 2 ||
		manifest.DuplicatesRemoved != 1 ||
		manifest.TrainExamples != 1 ||
		manifest.HoldoutExamples != 0 ||
		!manifest.ManualReviewNeeded ||
		manifest.TrainingAuthorized {
		t.Fatalf("manifest = %#v", manifest)
	}
	train, err := os.Open(filepath.Join(outputRoot, "train.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer train.Close()
	scanner := bufio.NewScanner(train)
	if !scanner.Scan() {
		t.Fatal("missing training example")
	}
	var example datasetExample
	if err := json.Unmarshal(scanner.Bytes(), &example); err != nil {
		t.Fatal(err)
	}
	if len(example.Messages) != 3 ||
		example.GroupID == "" ||
		!strings.Contains(example.Messages[1].Content, "go test failed") ||
		!strings.Contains(example.Messages[2].Content, "return 42") {
		t.Fatalf("example = %#v", example)
	}
	if scanner.Scan() {
		t.Fatal("unexpected second training example")
	}
	for _, name := range []string{"train.jsonl", "holdout.jsonl", "manifest.json"} {
		info, err := os.Stat(filepath.Join(outputRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
}

func TestBuildDatasetRejectsLikelySecret(t *testing.T) {
	inputRoot := t.TempDir()
	trace := datasetTestTrace(
		`password = "super-secret-password"`,
		`password = "another-secret-password"`,
	)
	writeTestJSON(t, filepath.Join(inputRoot, "secret.json"), trace)
	outputRoot := filepath.Join(t.TempDir(), "dataset")
	_, err := buildDataset(datasetOptions{
		Inputs: []string{inputRoot},
		Output: outputRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "möjlig hemlighet") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(outputRoot); !os.IsNotExist(statErr) {
		t.Fatalf("dataset directory was created after secret rejection: %v", statErr)
	}
}

func TestLatestDifferingDraftRejectsUnchangedTrace(t *testing.T) {
	files := []workerResultFile{{Path: "main.go", Content: "same"}}
	if _, ok := latestDifferingDraft(
		[]workerResultDraft{{Revision: 1, Files: files}},
		files,
	); ok {
		t.Fatal("unchanged draft was accepted as a repair transition")
	}
}

func TestDatasetUsesVerifiedFailingInitialFilesForDirectSuccess(t *testing.T) {
	trace := trainingTrace{
		SchemaVersion: trainingTraceSchemaVersion,
		Verdict:       "accepted",
		Task:          "repair Answer",
		Diagnostics:   "seed verification: failed\nfinal verification: passed",
		InitialFiles: []workerResultFile{{
			Path:    "answer.go",
			Content: "package answer\nfunc Answer() int { return 0 }\n",
		}},
		Drafts: []workerResultDraft{{
			Revision: 1,
			Files: []workerResultFile{{
				Path:    "answer.go",
				Content: "package answer\nfunc Answer() int { return 42 }\n",
			}},
		}},
		CorrectedFiles: []workerResultFile{{
			Path:    "answer.go",
			Content: "package answer\nfunc Answer() int { return 42 }\n",
		}},
	}
	example, usable, err := datasetExampleFromTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !usable || !strings.Contains(example.Messages[1].Content, "return 0") {
		t.Fatalf("example = %#v, usable = %t", example, usable)
	}
}

func TestDatasetSplitGroupsEquivalentRepairTargetsTogether(t *testing.T) {
	first := datasetTestTrace("return 0", "return 42")
	second := datasetTestTrace("return 0", "return 40 + 2")
	firstExample, firstOK, err := datasetExampleFromTrace(first)
	if err != nil || !firstOK {
		t.Fatalf("first example: %v, %t", err, firstOK)
	}
	secondExample, secondOK, err := datasetExampleFromTrace(second)
	if err != nil || !secondOK {
		t.Fatalf("second example: %v, %t", err, secondOK)
	}
	if firstExample.ID == secondExample.ID {
		t.Fatal("different targets received the same example ID")
	}
	if firstExample.GroupID != secondExample.GroupID {
		t.Fatalf("equivalent inputs split into groups %q and %q", firstExample.GroupID, secondExample.GroupID)
	}
}

func TestDatasetHoldoutSelectsExactGroupsWithoutLeakage(t *testing.T) {
	examples := []datasetExample{
		{ID: "a1", GroupID: "a"},
		{ID: "a2", GroupID: "a"},
		{ID: "b", GroupID: "b"},
		{ID: "c", GroupID: "c"},
		{ID: "d", GroupID: "d"},
		{ID: "e", GroupID: "e"},
	}
	holdout := selectDatasetHoldoutGroups(examples, 20)
	if len(holdout) != 1 || !holdout["a"] {
		t.Fatalf("holdout = %#v", holdout)
	}
	if holdout[examples[2].GroupID] {
		t.Fatal("unselected group leaked into holdout")
	}
}

func datasetTestTrace(before, after string) trainingTrace {
	return trainingTrace{
		SchemaVersion:  trainingTraceSchemaVersion,
		Verdict:        "accepted",
		Task:           "repair main.go",
		Role:           "implementer",
		WorkspaceID:    "dataset-test",
		JobID:          "job-test",
		Model:          "model-test",
		OriginalStatus: "completed",
		Diagnostics:    "go test failed: wrong answer",
		Drafts: []workerResultDraft{{
			Revision: 1,
			Files: []workerResultFile{{
				Path:    "main.go",
				Content: "package main\nfunc answer() int { " + before + " }\n",
			}},
		}},
		CorrectedFiles: []workerResultFile{{
			Path:    "main.go",
			Content: "package main\nfunc answer() int { " + after + " }\n",
		}},
	}
}
