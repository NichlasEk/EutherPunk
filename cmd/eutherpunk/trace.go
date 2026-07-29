package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	trainingTraceSchemaVersion = 1
	maxWorkerResultBytes       = 4 * 1024 * 1024
	maxTraceDiagnosticsBytes   = 64 * 1024
)

type traceFinalizeOptions struct {
	Result      string
	Workspace   string
	Diagnostics string
	Verdict     string
	Output      string
}

type trainingTrace struct {
	SchemaVersion    int                    `json:"schema_version"`
	CreatedAt        string                 `json:"created_at"`
	HarnessVersion   string                 `json:"harness_version"`
	Verdict          string                 `json:"verdict"`
	Task             string                 `json:"task"`
	Role             string                 `json:"role"`
	WorkspaceID      string                 `json:"workspace_id"`
	JobID            string                 `json:"job_id,omitempty"`
	Model            string                 `json:"model,omitempty"`
	OriginalStatus   string                 `json:"original_status"`
	OriginalMessage  string                 `json:"original_message,omitempty"`
	OriginalIssues   []string               `json:"original_issues,omitempty"`
	Diagnostics      string                 `json:"diagnostics"`
	Drafts           []workerResultDraft    `json:"drafts"`
	CandidateFiles   []workerResultFile     `json:"candidate_files,omitempty"`
	CorrectedFiles   []workerResultFile     `json:"corrected_files,omitempty"`
	Activities       []workspaceJobActivity `json:"activities,omitempty"`
	SourceResultHash string                 `json:"source_result_sha256"`
}

func runTrace(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "finalize" {
		return errors.New("använd eutherpunk trace finalize --help")
	}
	options, err := parseTraceFinalizeOptions(args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	trace, err := finalizeTrainingTrace(options)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := stdout.Write(raw); err != nil {
		return err
	}
	return writeTraceOutput(options.Output, raw)
}

func parseTraceFinalizeOptions(args []string, stderr io.Writer) (traceFinalizeOptions, error) {
	var options traceFinalizeOptions
	flags := flag.NewFlagSet("trace finalize", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.Result, "result", "", "worker result JSON")
	flags.StringVar(&options.Workspace, "workspace", "", "workspace containing the verified files")
	flags.StringVar(&options.Diagnostics, "diagnostics", "", "UTF-8 verifier diagnostics file")
	flags.StringVar(&options.Verdict, "verdict", "", "accepted or rejected")
	flags.StringVar(&options.Output, "output", "", "private training trace JSON")
	if err := flags.Parse(args); err != nil {
		return traceFinalizeOptions{}, err
	}
	if flags.NArg() != 0 {
		return traceFinalizeOptions{}, errors.New("trace finalize accepterar inga positionsargument")
	}
	options.Result = strings.TrimSpace(options.Result)
	options.Workspace = strings.TrimSpace(options.Workspace)
	options.Diagnostics = strings.TrimSpace(options.Diagnostics)
	options.Verdict = strings.ToLower(strings.TrimSpace(options.Verdict))
	options.Output = strings.TrimSpace(options.Output)
	if options.Result == "" || options.Workspace == "" ||
		options.Diagnostics == "" || options.Output == "" {
		return traceFinalizeOptions{}, errors.New("--result, --workspace, --diagnostics och --output krävs")
	}
	if options.Verdict != "accepted" && options.Verdict != "rejected" {
		return traceFinalizeOptions{}, errors.New("--verdict måste vara accepted eller rejected")
	}
	return options, nil
}

func finalizeTrainingTrace(options traceFinalizeOptions) (trainingTrace, error) {
	resultRaw, err := readPrivateTraceInput(options.Result, maxWorkerResultBytes)
	if err != nil {
		return trainingTrace{}, fmt.Errorf("läs worker-resultat: %w", err)
	}
	var result workerResult
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return trainingTrace{}, fmt.Errorf("tolka worker-resultat: %w", err)
	}
	if result.SchemaVersion != workerSchemaVersion {
		return trainingTrace{}, fmt.Errorf("worker-resultatets schemaversion %d stöds inte", result.SchemaVersion)
	}
	diagnosticsRaw, err := readPrivateTraceInput(options.Diagnostics, maxTraceDiagnosticsBytes)
	if err != nil {
		return trainingTrace{}, fmt.Errorf("läs diagnostik: %w", err)
	}
	if !utf8.Valid(diagnosticsRaw) || strings.TrimSpace(string(diagnosticsRaw)) == "" {
		return trainingTrace{}, errors.New("diagnostiken måste vara icke-tom UTF-8")
	}
	root, err := canonicalWorkspaceRoot(workspaceState{Root: options.Workspace})
	if err != nil {
		return trainingTrace{}, err
	}
	if result.Workspace != "" {
		resultRoot, err := canonicalWorkspaceRoot(workspaceState{Root: result.Workspace})
		if err != nil {
			return trainingTrace{}, fmt.Errorf("worker-resultatets arbetsyta: %w", err)
		}
		if resultRoot != root {
			return trainingTrace{}, errors.New("worker-resultatet tillhör en annan arbetsyta")
		}
	}
	drafts := append([]workerResultDraft(nil), result.Drafts...)
	if len(drafts) == 0 && len(result.Files) > 0 {
		drafts = []workerResultDraft{{
			Revision: result.CheckpointRevision,
			Files:    append([]workerResultFile(nil), result.Files...),
		}}
	}
	trace := trainingTrace{
		SchemaVersion:    trainingTraceSchemaVersion,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		HarnessVersion:   version,
		Verdict:          options.Verdict,
		Task:             result.Task,
		Role:             result.Role,
		WorkspaceID:      filepath.Base(root),
		JobID:            result.JobID,
		Model:            result.Model,
		OriginalStatus:   result.Status,
		OriginalMessage:  result.Message,
		OriginalIssues:   append([]string(nil), result.Issues...),
		Diagnostics:      strings.TrimSpace(string(diagnosticsRaw)),
		Drafts:           drafts,
		CandidateFiles:   append([]workerResultFile(nil), result.Files...),
		Activities:       append([]workspaceJobActivity(nil), result.Activities...),
		SourceResultHash: sha256Hex(resultRaw),
	}
	if options.Verdict == "accepted" {
		corrected, err := readCorrectedTraceFiles(root, result, drafts)
		if err != nil {
			return trainingTrace{}, err
		}
		trace.CorrectedFiles = corrected
	}
	return trace, nil
}

func readCorrectedTraceFiles(
	root string,
	result workerResult,
	drafts []workerResultDraft,
) ([]workerResultFile, error) {
	paths := make(map[string]struct{})
	for _, file := range result.Files {
		paths[file.Path] = struct{}{}
	}
	for _, draft := range drafts {
		for _, file := range draft.Files {
			paths[file.Path] = struct{}{}
		}
	}
	sortedPaths := make([]string, 0, len(paths))
	for path := range paths {
		sortedPaths = append(sortedPaths, path)
	}
	sort.Strings(sortedPaths)
	if len(sortedPaths) == 0 {
		return nil, errors.New("worker-resultatet innehåller inga målfiler")
	}
	files := make([]workerResultFile, 0, len(sortedPaths))
	for _, path := range sortedPaths {
		target, exists, err := safeWorkspaceTarget(root, path)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("korrigerad målfil saknas: %s", path)
		}
		raw, err := os.ReadFile(target)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxProposalFileBytes || !utf8.Valid(raw) {
			return nil, fmt.Errorf("korrigerad målfil är för stor eller inte UTF-8: %s", path)
		}
		files = append(files, workerResultFile{
			Path:    filepath.ToSlash(path),
			Bytes:   len(raw),
			SHA256:  sha256Hex(raw),
			Content: string(raw),
		})
	}
	return files, nil
}

func readPrivateTraceInput(path string, limit int) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("osäker indatafil %q", absolute)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s är större än %d byte", filepath.Base(absolute), limit)
	}
	return os.ReadFile(absolute)
}

func writeTraceOutput(path string, raw []byte) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("osäker trace-fil %q", absolute)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Stat(filepath.Dir(absolute)); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("trace-katalogen finns inte")
	}
	return writeProjectMemoryFileAtomic(absolute, raw)
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
