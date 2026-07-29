package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecuteEvalSuiteVerifiesAndFinalizesTrace(t *testing.T) {
	originalRunner := runEvalWorker
	originalRepair := runEvalRepair
	t.Cleanup(func() {
		runEvalWorker = originalRunner
		runEvalRepair = originalRepair
	})
	runEvalWorker = func(
		_ *cliConfig,
		args []string,
		stdout, _ io.Writer,
	) error {
		options, err := parseWorkerOptions(args, io.Discard)
		if err != nil {
			return err
		}
		if err := os.WriteFile(
			filepath.Join(options.Workspace, "answer.go"),
			[]byte("package evalcase\n\nfunc Answer() int { return 42 }\n"),
			0o600,
		); err != nil {
			return err
		}
		result := workerResult{
			SchemaVersion:      workerSchemaVersion,
			Status:             "completed",
			Task:               options.Task,
			Role:               options.Role,
			Workspace:          options.Workspace,
			Applied:            true,
			JobID:              "eval-job",
			Model:              "eval-model",
			CheckpointRevision: 1,
			Files: []workerResultFile{{
				Path:    "answer.go",
				Content: "package evalcase\n\nfunc Answer() int { return 42 }\n",
			}},
			Drafts: []workerResultDraft{{
				Revision: 1,
				Files: []workerResultFile{{
					Path:    "answer.go",
					Content: "package evalcase\n\nfunc Answer() int { return 42 }\n",
				}},
			}},
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
		}
		return writeWorkerResult(stdout, options.Output, result)
	}

	suite := evalSuite{
		SchemaVersion: evalSchemaVersion,
		Name:          "test-suite",
		Version:       "1",
		Cases: []evalCase{{
			ID:   "answer",
			Task: "repair Answer",
			Files: []evalSeedFile{
				{
					Path:    "go.mod",
					Content: "module eval.local/answer\n\ngo 1.22\n",
				},
				{
					Path:    "answer.go",
					Content: "package evalcase\n\nfunc Answer() int { return 0 }\n",
				},
				{
					Path:     "answer_test.go",
					Content:  "package evalcase\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) { if Answer() != 42 { t.Fatal(Answer()) } }\n",
					Preserve: true,
				},
			},
			Verifier: []string{"go", "test", "./..."},
		}},
	}
	outputRoot := filepath.Join(t.TempDir(), "results")
	var stdout, stderr bytes.Buffer
	if err := executeEvalSuite(
		&cliConfig{},
		suite,
		evalOptions{Output: outputRoot, Timeout: time.Minute},
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("executeEvalSuite: %v, stderr=%q", err, stderr.String())
	}
	var result evalRunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Model != "eval-model" ||
		result.SuiteSHA256 == "" ||
		result.Metrics.AcceptedCases != 1 ||
		result.Metrics.ExecutablePassRate != 1 ||
		len(result.Cases) != 1 ||
		result.Cases[0].DurationMS <= 0 {
		t.Fatalf("result = %#v", result)
	}
	traceRaw, err := os.ReadFile(filepath.Join(outputRoot, "answer", "trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var trace trainingTrace
	if err := json.Unmarshal(traceRaw, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Verdict != "accepted" ||
		trace.WorkspaceID != "workspace-answer" ||
		len(trace.Drafts) != 1 ||
		len(trace.CorrectedFiles) != 1 {
		t.Fatalf("trace = %#v", trace)
	}
}

func TestExecuteEvalSuiteRepairsFromVerifierDiagnostics(t *testing.T) {
	originalRunner := runEvalWorker
	originalRepair := runEvalRepair
	t.Cleanup(func() {
		runEvalWorker = originalRunner
		runEvalRepair = originalRepair
	})
	runEvalWorker = func(
		_ *cliConfig,
		args []string,
		stdout, _ io.Writer,
	) error {
		options, err := parseWorkerOptions(args, io.Discard)
		if err != nil {
			return err
		}
		result := workerResult{
			SchemaVersion:      workerSchemaVersion,
			Status:             "needs_review",
			Task:               options.Task,
			Role:               options.Role,
			Workspace:          options.Workspace,
			Applied:            true,
			JobID:              "repair-job",
			Model:              "eval-model",
			CheckpointRevision: 1,
			Files: []workerResultFile{{
				Path:    "answer.go",
				Content: "package evalcase\n\nfunc Answer() int { return 0 }\n",
			}},
			Drafts: []workerResultDraft{{
				Revision: 1,
				Files: []workerResultFile{{
					Path:    "answer.go",
					Content: "package evalcase\n\nfunc Answer() int { return 0 }\n",
				}},
			}},
		}
		return writeWorkerResult(stdout, options.Output, result)
	}
	var repairDiagnostics string
	runEvalRepair = func(
		_ *cliConfig,
		workspaceRoot string,
		worker workerResult,
		diagnostics string,
		_ time.Duration,
		_ io.Writer,
	) (workerResult, error) {
		repairDiagnostics = diagnostics
		content := "package evalcase\n\nfunc Answer() int { return 42 }\n"
		if err := os.WriteFile(
			filepath.Join(workspaceRoot, "answer.go"),
			[]byte(content),
			0o600,
		); err != nil {
			return worker, err
		}
		worker.CheckpointRevision = 2
		worker.Files = []workerResultFile{{Path: "answer.go", Content: content}}
		worker.Drafts = append(worker.Drafts, workerResultDraft{
			Revision: 2,
			Files:    []workerResultFile{{Path: "answer.go", Content: content}},
		})
		return worker, nil
	}

	suite := evalSuite{
		SchemaVersion: evalSchemaVersion,
		Name:          "repair-suite",
		Version:       "1",
		Cases: []evalCase{{
			ID:   "answer-repair",
			Task: "repair Answer",
			Files: []evalSeedFile{
				{Path: "go.mod", Content: "module eval.local/answerrepair\n\ngo 1.22\n"},
				{Path: "answer.go", Content: "package evalcase\n\nfunc Answer() int { return 0 }\n"},
				{
					Path:     "answer_test.go",
					Content:  "package evalcase\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) { if Answer() != 42 { t.Fatal(Answer()) } }\n",
					Preserve: true,
				},
			},
			Verifier: []string{"go", "test", "./..."},
		}},
	}
	outputRoot := filepath.Join(t.TempDir(), "results")
	var stdout bytes.Buffer
	if err := executeEvalSuite(
		&cliConfig{},
		suite,
		evalOptions{Output: outputRoot, Timeout: time.Minute},
		&stdout,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}
	var result evalRunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Cases) != 1 ||
		result.Cases[0].Status != "accepted" ||
		result.Cases[0].RepairRounds != 1 ||
		!result.Cases[0].VerifierPassed {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Contains([]byte(repairDiagnostics), []byte("TestAnswer")) {
		t.Fatalf("repair diagnostics = %q", repairDiagnostics)
	}
	diagnostics, err := os.ReadFile(
		filepath.Join(outputRoot, "answer-repair", "diagnostics.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(diagnostics, []byte("verification round 0")) ||
		!bytes.Contains(diagnostics, []byte("verification round 1")) {
		t.Fatalf("diagnostic history = %q", diagnostics)
	}
}

func TestValidateEvalCaseRejectsShellVerifier(t *testing.T) {
	err := validateEvalCase(evalCase{
		ID:   "unsafe",
		Task: "do something",
		Files: []evalSeedFile{{
			Path:    "main.go",
			Content: "package main\n",
		}},
		Verifier: []string{"sh", "-c", "anything"},
	})
	if err == nil {
		t.Fatal("shell verifier was accepted")
	}
}

func TestLoadFrozenEvalSuite(t *testing.T) {
	suite, err := loadEvalSuite(filepath.Join("..", "..", "evaluation", "v1", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "eutherpunk-repair-core" || len(suite.Cases) != 3 {
		t.Fatalf("suite = %#v", suite)
	}

	multilang, err := loadEvalSuite(filepath.Join("..", "..", "evaluation", "v2", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	if multilang.Name != "eutherpunk-repair-multilang" || len(multilang.Cases) != 7 {
		t.Fatalf("multilang suite = %#v", multilang)
	}

	diverse, err := loadEvalSuite(filepath.Join("..", "..", "evaluation", "v3", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	if diverse.Name != "eutherpunk-repair-diverse" || len(diverse.Cases) != 20 {
		t.Fatalf("diverse suite = %#v", diverse)
	}

	creative, err := loadEvalSuite(
		filepath.Join("..", "..", "evaluation", "creative-v1", "suite.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if creative.Name != "eutherpunk-neon-life-repair" || len(creative.Cases) != 1 {
		t.Fatalf("creative suite = %#v", creative)
	}
}
