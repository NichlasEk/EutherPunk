package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	evalSchemaVersion  = 1
	maxEvalSuiteBytes  = 512 * 1024
	maxEvalCases       = 64
	maxEvalVerifyBytes = 64 * 1024
)

type evalOptions struct {
	Suite   string
	Output  string
	CaseID  string
	Timeout time.Duration
}

type evalSuite struct {
	SchemaVersion int        `json:"schema_version"`
	Name          string     `json:"name"`
	Version       string     `json:"version"`
	Cases         []evalCase `json:"cases"`
}

type evalCase struct {
	ID       string         `json:"id"`
	Task     string         `json:"task"`
	Files    []evalSeedFile `json:"files"`
	Verifier []string       `json:"verifier"`
}

type evalSeedFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Preserve bool   `json:"preserve,omitempty"`
}

type evalRunResult struct {
	SchemaVersion int              `json:"schema_version"`
	SuiteName     string           `json:"suite_name"`
	SuiteVersion  string           `json:"suite_version"`
	SuiteSHA256   string           `json:"suite_sha256"`
	Harness       string           `json:"harness_version"`
	Model         string           `json:"model,omitempty"`
	StartedAt     string           `json:"started_at"`
	FinishedAt    string           `json:"finished_at"`
	Cases         []evalCaseResult `json:"cases"`
	Metrics       evalMetrics      `json:"metrics"`
}

type evalCaseResult struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	WorkerStatus       string   `json:"worker_status,omitempty"`
	JobID              string   `json:"job_id,omitempty"`
	Model              string   `json:"model,omitempty"`
	VerifierPassed     bool     `json:"verifier_passed"`
	PreservationPassed bool     `json:"preservation_passed"`
	RepairRounds       int      `json:"repair_rounds"`
	ChangedFiles       []string `json:"changed_files,omitempty"`
	PreservationErrors []string `json:"preservation_errors,omitempty"`
	WorkerResult       string   `json:"worker_result"`
	Diagnostics        string   `json:"diagnostics"`
	Trace              string   `json:"trace"`
	DurationMS         int64    `json:"duration_ms"`
	Error              string   `json:"error,omitempty"`
}

type evalMetrics struct {
	TotalCases            int     `json:"total_cases"`
	AcceptedCases         int     `json:"accepted_cases"`
	ExecutablePassRate    float64 `json:"executable_pass_rate"`
	PreservationPassRate  float64 `json:"preservation_pass_rate"`
	HarnessCompletionRate float64 `json:"harness_completion_rate"`
	MeanDurationMS        int64   `json:"mean_duration_ms"`
}

var (
	runEvalWorker = runWorker
	runEvalRepair = runEvalVerifiedRepair
)

func runEval(cfg *cliConfig, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("använd eutherpunk eval run --help")
	}
	options, err := parseEvalOptions(args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	suite, err := loadEvalSuite(options.Suite)
	if err != nil {
		return err
	}
	return executeEvalSuite(cfg, suite, options, stdout, stderr)
}

func parseEvalOptions(args []string, stderr io.Writer) (evalOptions, error) {
	options := evalOptions{Timeout: defaultWorkerTimeout}
	flags := flag.NewFlagSet("eval run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.Suite, "suite", "", "trusted frozen evaluation suite JSON")
	flags.StringVar(&options.Output, "output", "", "new private result directory")
	flags.StringVar(&options.CaseID, "case", "", "run only one case ID")
	flags.DurationVar(&options.Timeout, "timeout", options.Timeout, "worker timeout per case (maximum 30m)")
	if err := flags.Parse(args); err != nil {
		return evalOptions{}, err
	}
	if flags.NArg() != 0 {
		return evalOptions{}, errors.New("eval run accepterar inga positionsargument")
	}
	options.Suite = strings.TrimSpace(options.Suite)
	options.Output = strings.TrimSpace(options.Output)
	options.CaseID = strings.TrimSpace(options.CaseID)
	if options.Suite == "" || options.Output == "" {
		return evalOptions{}, errors.New("--suite och --output krävs")
	}
	if options.Timeout <= 0 || options.Timeout > maxWorkerTimeout {
		return evalOptions{}, errors.New("--timeout måste vara större än 0 och högst 30m")
	}
	return options, nil
}

func loadEvalSuite(path string) (evalSuite, error) {
	raw, err := readPrivateTraceInput(path, maxEvalSuiteBytes)
	if err != nil {
		return evalSuite{}, fmt.Errorf("läs eval-svit: %w", err)
	}
	var suite evalSuite
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&suite); err != nil {
		return evalSuite{}, fmt.Errorf("tolka eval-svit: %w", err)
	}
	if suite.SchemaVersion != evalSchemaVersion {
		return evalSuite{}, fmt.Errorf("eval-svitens schemaversion %d stöds inte", suite.SchemaVersion)
	}
	if strings.TrimSpace(suite.Name) == "" || strings.TrimSpace(suite.Version) == "" {
		return evalSuite{}, errors.New("eval-sviten saknar namn eller version")
	}
	if len(suite.Cases) == 0 || len(suite.Cases) > maxEvalCases {
		return evalSuite{}, fmt.Errorf("eval-sviten måste innehålla 1-%d fall", maxEvalCases)
	}
	seen := make(map[string]bool, len(suite.Cases))
	for index := range suite.Cases {
		if err := validateEvalCase(suite.Cases[index]); err != nil {
			return evalSuite{}, fmt.Errorf("eval-fall %d: %w", index+1, err)
		}
		if seen[suite.Cases[index].ID] {
			return evalSuite{}, fmt.Errorf("duplicerat eval-ID %q", suite.Cases[index].ID)
		}
		seen[suite.Cases[index].ID] = true
	}
	return suite, nil
}

func validateEvalCase(test evalCase) error {
	if test.ID == "" || strings.ContainsAny(test.ID, `/\`) || filepath.Base(test.ID) != test.ID {
		return errors.New("ID måste vara ett enkelt filnamn")
	}
	if strings.TrimSpace(test.Task) == "" {
		return errors.New("uppgift saknas")
	}
	if len(test.Files) == 0 || len(test.Files) > maxWorkspaceFiles {
		return errors.New("ogiltigt antal startfiler")
	}
	paths := make(map[string]bool, len(test.Files))
	for _, file := range test.Files {
		if err := validateProposedFile(workspaceFile{Path: file.Path, Content: file.Content}); err != nil {
			return err
		}
		if paths[file.Path] {
			return fmt.Errorf("duplicerad startfil %q", file.Path)
		}
		paths[file.Path] = true
	}
	if len(test.Verifier) == 0 || !allowedEvalVerifier(test.Verifier[0]) {
		return errors.New("verifieraren saknas eller är inte tillåten")
	}
	for _, arg := range test.Verifier {
		if strings.ContainsRune(arg, '\x00') || len(arg) > 512 {
			return errors.New("ogiltigt verifierarargument")
		}
	}
	return nil
}

func allowedEvalVerifier(program string) bool {
	switch program {
	case "go", "node", "cargo", "lua", "luac":
		return true
	default:
		return false
	}
}

func executeEvalSuite(
	cfg *cliConfig,
	suite evalSuite,
	options evalOptions,
	stdout, stderr io.Writer,
) error {
	outputRoot, err := filepath.Abs(options.Output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(outputRoot); err == nil {
		return fmt.Errorf("resultatkatalogen finns redan: %s", outputRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(outputRoot, 0o700); err != nil {
		return err
	}
	started := time.Now().UTC()
	result := evalRunResult{
		SchemaVersion: evalSchemaVersion,
		SuiteName:     suite.Name,
		SuiteVersion:  suite.Version,
		SuiteSHA256:   evalSuiteHash(suite),
		Harness:       version,
		StartedAt:     started.Format(time.RFC3339),
	}
	selected := 0
	for _, test := range suite.Cases {
		if options.CaseID != "" && test.ID != options.CaseID {
			continue
		}
		selected++
		caseResult := executeEvalCase(cfg, outputRoot, test, options.Timeout, stderr)
		if result.Model == "" {
			result.Model = caseResult.Model
		}
		result.Cases = append(result.Cases, caseResult)
		if err := saveEvalSummary(outputRoot, result); err != nil {
			return err
		}
	}
	if selected == 0 {
		return fmt.Errorf("eval-fallet %q finns inte i sviten", options.CaseID)
	}
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	result.Metrics = calculateEvalMetrics(result.Cases)
	if err := saveEvalSummary(outputRoot, result); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = stdout.Write(raw)
	return err
}

func executeEvalCase(
	cfg *cliConfig,
	outputRoot string,
	test evalCase,
	timeout time.Duration,
	stderr io.Writer,
) (result evalCaseResult) {
	started := time.Now()
	caseRoot := filepath.Join(outputRoot, test.ID)
	workspaceRoot := filepath.Join(caseRoot, "workspace-"+test.ID)
	resultPath := filepath.Join(caseRoot, "worker.json")
	diagnosticsPath := filepath.Join(caseRoot, "diagnostics.txt")
	tracePath := filepath.Join(caseRoot, "trace.json")
	result = evalCaseResult{
		ID:                 test.ID,
		Status:             "rejected",
		PreservationPassed: true,
		WorkerResult:       relativeEvalPath(outputRoot, resultPath),
		Diagnostics:        relativeEvalPath(outputRoot, diagnosticsPath),
		Trace:              relativeEvalPath(outputRoot, tracePath),
	}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		result.Error = err.Error()
		return result
	}
	initial := make(map[string]string, len(test.Files))
	preserved := make(map[string]string)
	for _, file := range test.Files {
		if err := writeWorkspaceFile(workspaceRoot, workspaceFile{
			Path: file.Path, Content: file.Content,
		}); err != nil {
			result.Error = err.Error()
			return result
		}
		initial[file.Path] = evalContentHash(file.Content)
		if file.Preserve {
			preserved[file.Path] = initial[file.Path]
		}
	}
	seedOutput, seedErr := runEvalVerifier(workspaceRoot, test.Verifier)
	seedDiagnostic := formatEvalDiagnosticRound(
		-1,
		test.Verifier,
		seedOutput,
		seedErr,
		nil,
	)
	if seedErr == nil {
		result.Error = "eval-startläget passerar redan verifieraren"
		_ = writeProjectMemoryFileAtomic(diagnosticsPath, []byte(seedDiagnostic))
		return result
	}
	_, _ = fmt.Fprintf(stderr, "eval %s: worker startar\n", test.ID)
	var workerJSON bytes.Buffer
	workerErr := runEvalWorker(
		cfg,
		[]string{
			"--workspace", workspaceRoot,
			"--task", test.Task,
			"--output", resultPath,
			"--apply",
			"--verifier-driven",
			"--timeout", timeout.String(),
		},
		&workerJSON,
		stderr,
	)
	var worker workerResult
	if err := json.Unmarshal(workerJSON.Bytes(), &worker); err != nil {
		result.Error = fmt.Sprintf("tolka worker-resultat: %v", err)
		return result
	}
	result.WorkerStatus = worker.Status
	result.JobID = worker.JobID
	result.Model = worker.Model

	verifyOutput, verifierErr := runEvalVerifier(workspaceRoot, test.Verifier)
	diagnosticRounds := []string{
		seedDiagnostic,
		formatEvalDiagnosticRound(0, test.Verifier, verifyOutput, verifierErr, workerErr),
	}
	for verifierErr != nil &&
		result.RepairRounds < 2 &&
		worker.JobID != "" &&
		workerResultCanRepair(worker) &&
		len(worker.Files) > 0 {
		result.RepairRounds++
		_, _ = fmt.Fprintf(
			stderr,
			"eval %s: verifierad reparation %d startar\n",
			test.ID,
			result.RepairRounds,
		)
		diagnostic := formatEvalDiagnostics(test.Verifier, verifyOutput, verifierErr, nil)
		updated, repairErr := runEvalRepair(
			cfg,
			workspaceRoot,
			worker,
			diagnostic,
			timeout,
			stderr,
		)
		if repairErr != nil {
			diagnosticRounds = append(
				diagnosticRounds,
				fmt.Sprintf("repair round %d: failed: %v\n", result.RepairRounds, repairErr),
			)
			break
		}
		worker = updated
		if err := writeEvalWorkerResult(resultPath, worker); err != nil {
			result.Error = err.Error()
			return result
		}
		result.WorkerStatus = worker.Status
		result.Model = worker.Model
		verifyOutput, verifierErr = runEvalVerifier(workspaceRoot, test.Verifier)
		diagnosticRounds = append(
			diagnosticRounds,
			formatEvalDiagnosticRound(
				result.RepairRounds,
				test.Verifier,
				verifyOutput,
				verifierErr,
				nil,
			),
		)
	}
	result.VerifierPassed = verifierErr == nil
	result.ChangedFiles = changedEvalFiles(workspaceRoot, initial)
	result.PreservationErrors = checkEvalPreservation(workspaceRoot, preserved)
	result.PreservationPassed = len(result.PreservationErrors) == 0
	diagnostics := strings.Join(diagnosticRounds, "\n")
	if err := writeProjectMemoryFileAtomic(diagnosticsPath, []byte(diagnostics)); err != nil {
		result.Error = err.Error()
		return result
	}
	verdict := "rejected"
	if result.VerifierPassed && result.PreservationPassed {
		verdict = "accepted"
		result.Status = "accepted"
	}
	trace, traceErr := finalizeTrainingTrace(traceFinalizeOptions{
		Result:      resultPath,
		Workspace:   workspaceRoot,
		Diagnostics: diagnosticsPath,
		Verdict:     verdict,
		Output:      tracePath,
	})
	if traceErr == nil {
		var raw []byte
		raw, traceErr = json.MarshalIndent(trace, "", "  ")
		if traceErr == nil {
			traceErr = writeProjectMemoryFileAtomic(tracePath, append(raw, '\n'))
		}
	}
	if traceErr != nil {
		result.Error = fmt.Sprintf("slutför trace: %v", traceErr)
	} else if workerErr != nil {
		result.Error = workerErr.Error()
	}
	_, _ = fmt.Fprintf(
		stderr,
		"eval %s: %s (verifierare=%t, bevarande=%t)\n",
		test.ID,
		result.Status,
		result.VerifierPassed,
		result.PreservationPassed,
	)
	return result
}

func workerResultCanRepair(worker workerResult) bool {
	return worker.Status == "completed" || worker.Status == "needs_review"
}

func runEvalVerifiedRepair(
	cfg *cliConfig,
	workspaceRoot string,
	worker workerResult,
	diagnostics string,
	timeout time.Duration,
	stderr io.Writer,
) (workerResult, error) {
	if len(diagnostics) > 16*1024 {
		diagnostics = diagnostics[:16*1024]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	job, err := requestWorkspaceJobRepair(ctx, *cfg, worker.JobID, diagnostics)
	if err != nil {
		return worker, err
	}
	current := job
	lastActivity := 1
	if count := len(worker.Activities); count > 0 {
		lastActivity = worker.Activities[count-1].Sequence + 1
	}
	lastDraftRevision := worker.CheckpointRevision
	backedUpPaths := map[string]bool{}
	message, proposal, waitErr := waitWorkspaceJob(
		ctx,
		*cfg,
		job,
		stderr,
		&lastActivity,
		func(update workspaceJobResponse) error {
			current = update
			if update.DraftRev <= lastDraftRevision {
				return nil
			}
			if len(update.DraftFiles) == 0 {
				return errors.New("reparationen rapporterade en arbetskopierevision utan filer")
			}
			if err := applyWorkspaceDraft(
				workspaceState{Root: workspaceRoot},
				update.DraftFiles,
				update.DraftRev,
				backedUpPaths,
				stderr,
			); err != nil {
				return err
			}
			lastDraftRevision = update.DraftRev
			return nil
		},
	)
	worker.JobID = current.ID
	worker.Model = current.Model
	worker.Message = message
	worker.CheckpointRevision = current.DraftRev
	worker.Activities = append([]workspaceJobActivity(nil), current.Activities...)
	worker.Issues = projectReviewIssues(current.Activities)
	resultFiles := proposal.Files
	if len(resultFiles) == 0 && current.DraftRev > 0 {
		resultFiles = current.DraftFiles
	}
	worker.Files = workerResultFiles(resultFiles)
	worker.Drafts = workerResultDrafts(current.Drafts)
	worker.Applied = worker.Applied || current.DraftRev > 0
	worker.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	switch {
	case waitErr != nil:
		worker.Status = "failed"
		worker.Error = waitErr.Error()
	case len(proposal.Files) > 0:
		worker.Status = "completed"
		worker.Error = ""
	case current.DraftRev > 0:
		worker.Status = "needs_review"
		worker.Error = ""
	default:
		worker.Status = "no_change"
		worker.Error = ""
	}
	return worker, waitErr
}

func writeEvalWorkerResult(path string, result workerResult) error {
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return writeProjectMemoryFileAtomic(path, append(raw, '\n'))
}

func runEvalVerifier(root string, command []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = root
	cmd.Env = append(
		os.Environ(),
		"GOCACHE="+filepath.Join(filepath.Dir(root), ".eval-go-cache"),
	)
	var output limitedEvalBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output.String(), errors.New("verifieraren överskred 2 minuter")
	}
	return output.String(), err
}

type limitedEvalBuffer struct {
	data []byte
}

func (buffer *limitedEvalBuffer) Write(raw []byte) (int, error) {
	original := len(raw)
	remaining := maxEvalVerifyBytes - len(buffer.data)
	if remaining > 0 {
		if len(raw) > remaining {
			raw = raw[:remaining]
		}
		buffer.data = append(buffer.data, raw...)
	}
	return original, nil
}

func (buffer *limitedEvalBuffer) String() string {
	if !utf8.Valid(buffer.data) {
		return strings.ToValidUTF8(string(buffer.data), "\uFFFD")
	}
	return string(buffer.data)
}

func changedEvalFiles(root string, initial map[string]string) []string {
	var changed []string
	for path, originalHash := range initial {
		target := filepath.Join(root, filepath.FromSlash(path))
		raw, err := os.ReadFile(target)
		if err != nil || evalContentHash(string(raw)) != originalHash {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func checkEvalPreservation(root string, preserved map[string]string) []string {
	var issues []string
	for path, originalHash := range preserved {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			issues = append(issues, path+": missing or unreadable")
			continue
		}
		if evalContentHash(string(raw)) != originalHash {
			issues = append(issues, path+": changed")
		}
	}
	sort.Strings(issues)
	return issues
}

func formatEvalDiagnostics(command []string, output string, verifierErr, workerErr error) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "command: %s\n", strings.Join(command, " "))
	if verifierErr == nil {
		builder.WriteString("verifier: passed\n")
	} else {
		fmt.Fprintf(&builder, "verifier: failed: %v\n", verifierErr)
	}
	if workerErr != nil {
		fmt.Fprintf(&builder, "worker: %v\n", workerErr)
	}
	if strings.TrimSpace(output) != "" {
		builder.WriteString("\noutput:\n")
		builder.WriteString(strings.TrimSpace(output))
		builder.WriteByte('\n')
	}
	return builder.String()
}

func formatEvalDiagnosticRound(
	round int,
	command []string,
	output string,
	verifierErr, workerErr error,
) string {
	label := fmt.Sprintf("verification round %d", round)
	if round < 0 {
		label = "seed verification"
	}
	return fmt.Sprintf(
		"%s:\n%s",
		label,
		formatEvalDiagnostics(command, output, verifierErr, workerErr),
	)
}

func calculateEvalMetrics(cases []evalCaseResult) evalMetrics {
	metrics := evalMetrics{TotalCases: len(cases)}
	var duration int64
	var executable, preserved, completed int
	for _, test := range cases {
		duration += test.DurationMS
		if test.Status == "accepted" {
			metrics.AcceptedCases++
		}
		if test.VerifierPassed {
			executable++
		}
		if test.PreservationPassed {
			preserved++
		}
		if test.WorkerStatus == "completed" {
			completed++
		}
	}
	if len(cases) > 0 {
		count := float64(len(cases))
		metrics.ExecutablePassRate = float64(executable) / count
		metrics.PreservationPassRate = float64(preserved) / count
		metrics.HarnessCompletionRate = float64(completed) / count
		metrics.MeanDurationMS = duration / int64(len(cases))
	}
	return metrics
}

func saveEvalSummary(outputRoot string, result evalRunResult) error {
	result.Metrics = calculateEvalMetrics(result.Cases)
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return writeProjectMemoryFileAtomic(
		filepath.Join(outputRoot, "summary.json"),
		append(raw, '\n'),
	)
}

func relativeEvalPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

func evalContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func evalSuiteHash(suite evalSuite) string {
	raw, err := json.Marshal(suite)
	if err != nil {
		return ""
	}
	return evalContentHash(string(raw))
}
