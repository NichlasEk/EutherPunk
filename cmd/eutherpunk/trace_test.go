package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceFinalizeCapturesDraftAndVerifiedCorrection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package main\nfunc run() {}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	result := workerResult{
		SchemaVersion:      workerSchemaVersion,
		Status:             "needs_review",
		Task:               "fix main.go",
		Role:               "implementer",
		Workspace:          root,
		JobID:              "job-1",
		Model:              "devstral-test",
		Message:            "review failed",
		Issues:             []string{"main.go: undefined run"},
		CheckpointRevision: 2,
		InitialFiles: []workerResultFile{{
			Path:    "main.go",
			Content: "package main\n",
		}},
		Files: []workerResultFile{{
			Path:    "main.go",
			Bytes:   37,
			SHA256:  "draft-hash",
			Content: "package main\nfunc main() { run() }\n",
		}},
		Drafts: []workerResultDraft{
			{
				Revision: 1,
				Files: []workerResultFile{{
					Path:    "main.go",
					Content: "package main\n",
				}},
			},
			{
				Revision: 2,
				Files: []workerResultFile{{
					Path:    "main.go",
					Content: "package main\nfunc main() { run() }\n",
				}},
			},
		},
	}
	resultPath := filepath.Join(t.TempDir(), "worker.json")
	writeTestJSON(t, resultPath, result)
	diagnosticsPath := filepath.Join(t.TempDir(), "diagnostics.txt")
	if err := os.WriteFile(
		diagnosticsPath,
		[]byte("go test ./...\nmain.go: undefined: run\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "trace.json")
	var stdout, stderr bytes.Buffer
	if err := runTrace(
		[]string{
			"finalize",
			"--result", resultPath,
			"--workspace", root,
			"--diagnostics", diagnosticsPath,
			"--verdict", "accepted",
			"--output", outputPath,
		},
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runTrace: %v, stderr=%q", err, stderr.String())
	}
	fromFile, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), fromFile) {
		t.Fatal("stdout and trace output differ")
	}
	var trace trainingTrace
	if err := json.Unmarshal(fromFile, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Verdict != "accepted" ||
		trace.WorkspaceID != filepath.Base(root) ||
		len(trace.InitialFiles) != 1 ||
		len(trace.Drafts) != 2 ||
		len(trace.CorrectedFiles) != 1 {
		t.Fatalf("trace = %#v", trace)
	}
	if trace.CorrectedFiles[0].Content != "package main\nfunc run() {}\n" {
		t.Fatalf("corrected file = %#v", trace.CorrectedFiles[0])
	}
	if trace.SourceResultHash == "" ||
		!strings.Contains(trace.Diagnostics, "undefined: run") {
		t.Fatalf("trace provenance = %#v", trace)
	}
	if strings.Contains(string(fromFile), root) {
		t.Fatal("trace leaked the absolute workspace path")
	}
}

func TestTraceFinalizeRejectsDifferentWorkspace(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "worker.json")
	writeTestJSON(t, resultPath, workerResult{
		SchemaVersion: workerSchemaVersion,
		Status:        "needs_review",
		Task:          "fix",
		Role:          "implementer",
		Workspace:     firstRoot,
		Files: []workerResultFile{{
			Path:    "main.go",
			Content: "broken",
		}},
	})
	diagnosticsPath := filepath.Join(t.TempDir(), "diagnostics.txt")
	if err := os.WriteFile(diagnosticsPath, []byte("failed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := finalizeTrainingTrace(traceFinalizeOptions{
		Result:      resultPath,
		Workspace:   secondRoot,
		Diagnostics: diagnosticsPath,
		Verdict:     "rejected",
		Output:      filepath.Join(t.TempDir(), "trace.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "annan arbetsyta") {
		t.Fatalf("error = %v", err)
	}
}

func TestTraceFinalizeRejectsSymlinkedInput(t *testing.T) {
	target := filepath.Join(t.TempDir(), "worker.json")
	writeTestJSON(t, target, workerResult{SchemaVersion: workerSchemaVersion})
	link := filepath.Join(t.TempDir(), "worker-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTraceInput(link, maxWorkerResultBytes); err == nil {
		t.Fatal("symlinked trace input accepted")
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
