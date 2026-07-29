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
	t.Cleanup(func() { runEvalWorker = originalRunner })
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
		len(trace.Drafts) != 1 ||
		len(trace.CorrectedFiles) != 1 {
		t.Fatalf("trace = %#v", trace)
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
}
