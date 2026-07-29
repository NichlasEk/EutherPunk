package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	datasetSchemaVersion = 1
	maxDatasetInputs     = 16
	maxDatasetTraces     = 1000
)

type datasetOptions struct {
	Inputs         stringListFlag
	Output         string
	HoldoutPercent int
}

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("tom --input")
	}
	*values = append(*values, value)
	return nil
}

type datasetExample struct {
	SchemaVersion int              `json:"schema_version"`
	ID            string           `json:"id"`
	SourceModel   string           `json:"source_model,omitempty"`
	SourceJobID   string           `json:"source_job_id,omitempty"`
	Messages      []datasetMessage `json:"messages"`
}

type datasetMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type datasetManifest struct {
	SchemaVersion       int      `json:"schema_version"`
	CreatedAt           string   `json:"created_at"`
	HarnessVersion      string   `json:"harness_version"`
	InputPaths          []string `json:"input_paths"`
	TraceFilesInspected int      `json:"trace_files_inspected"`
	AcceptedTraces      int      `json:"accepted_traces"`
	RepairTransitions   int      `json:"repair_transitions"`
	DuplicatesRemoved   int      `json:"duplicates_removed"`
	TrainExamples       int      `json:"train_examples"`
	HoldoutExamples     int      `json:"holdout_examples"`
	HoldoutPercent      int      `json:"holdout_percent"`
	ManualReviewNeeded  bool     `json:"manual_license_and_secret_review_required"`
	TrainingAuthorized  bool     `json:"training_authorized"`
}

var datasetSecretPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`)},
	{"JWT bearer token", regexp.MustCompile(`(?i)\bbearer\s+eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"assigned secret", regexp.MustCompile(`(?i)\b(password|passwd|api[_-]?key|access[_-]?token|client[_-]?secret)\b\s*[:=]\s*["'][^"'\r\n]{8,}["']`)},
}

func runDataset(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "build" {
		return errors.New("använd eutherpunk dataset build --help")
	}
	options, err := parseDatasetOptions(args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	manifest, err := buildDataset(options)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(raw, '\n'))
	return err
}

func parseDatasetOptions(args []string, stderr io.Writer) (datasetOptions, error) {
	options := datasetOptions{HoldoutPercent: 20}
	flags := flag.NewFlagSet("dataset build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&options.Inputs, "input", "private trace JSON file or directory (repeatable)")
	flags.StringVar(&options.Output, "output", "", "new private dataset directory")
	flags.IntVar(&options.HoldoutPercent, "holdout-percent", options.HoldoutPercent, "deterministic holdout percentage")
	if err := flags.Parse(args); err != nil {
		return datasetOptions{}, err
	}
	if flags.NArg() != 0 {
		return datasetOptions{}, errors.New("dataset build accepterar inga positionsargument")
	}
	options.Output = strings.TrimSpace(options.Output)
	if len(options.Inputs) == 0 || len(options.Inputs) > maxDatasetInputs || options.Output == "" {
		return datasetOptions{}, fmt.Errorf("1-%d --input och --output krävs", maxDatasetInputs)
	}
	if options.HoldoutPercent < 0 || options.HoldoutPercent > 50 {
		return datasetOptions{}, errors.New("--holdout-percent måste vara 0-50")
	}
	return options, nil
}

func buildDataset(options datasetOptions) (datasetManifest, error) {
	outputRoot, err := filepath.Abs(options.Output)
	if err != nil {
		return datasetManifest{}, err
	}
	if _, err := os.Lstat(outputRoot); err == nil {
		return datasetManifest{}, fmt.Errorf("datasetkatalogen finns redan: %s", outputRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return datasetManifest{}, err
	}
	tracePaths, displayInputs, err := discoverDatasetTraces(options.Inputs)
	if err != nil {
		return datasetManifest{}, err
	}
	manifest := datasetManifest{
		SchemaVersion:       datasetSchemaVersion,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
		HarnessVersion:      version,
		InputPaths:          displayInputs,
		TraceFilesInspected: len(tracePaths),
		HoldoutPercent:      options.HoldoutPercent,
		ManualReviewNeeded:  true,
		TrainingAuthorized:  false,
	}
	examples := make(map[string]datasetExample)
	for _, path := range tracePaths {
		raw, err := readPrivateTraceInput(path, maxWorkerResultBytes)
		if err != nil {
			return datasetManifest{}, fmt.Errorf("läs %s: %w", filepath.Base(path), err)
		}
		var trace trainingTrace
		if err := json.Unmarshal(raw, &trace); err != nil {
			return datasetManifest{}, fmt.Errorf("tolka %s: %w", filepath.Base(path), err)
		}
		if trace.SchemaVersion != trainingTraceSchemaVersion || trace.Verdict == "" {
			continue
		}
		if trace.Verdict != "accepted" || len(trace.CorrectedFiles) == 0 {
			continue
		}
		manifest.AcceptedTraces++
		example, usable, err := datasetExampleFromTrace(trace)
		if err != nil {
			return datasetManifest{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if !usable {
			continue
		}
		manifest.RepairTransitions++
		if _, exists := examples[example.ID]; exists {
			manifest.DuplicatesRemoved++
			continue
		}
		examples[example.ID] = example
	}
	if len(examples) == 0 {
		return datasetManifest{}, errors.New("inga verifierade reparationsövergångar hittades")
	}
	sorted := make([]datasetExample, 0, len(examples))
	for _, example := range examples {
		sorted = append(sorted, example)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var train, holdout []datasetExample
	for _, example := range sorted {
		if len(sorted) >= 5 && datasetHoldout(example.ID, options.HoldoutPercent) {
			holdout = append(holdout, example)
		} else {
			train = append(train, example)
		}
	}
	manifest.TrainExamples = len(train)
	manifest.HoldoutExamples = len(holdout)
	if err := os.MkdirAll(outputRoot, 0o700); err != nil {
		return datasetManifest{}, err
	}
	if err := writeDatasetJSONL(filepath.Join(outputRoot, "train.jsonl"), train); err != nil {
		return datasetManifest{}, err
	}
	if err := writeDatasetJSONL(filepath.Join(outputRoot, "holdout.jsonl"), holdout); err != nil {
		return datasetManifest{}, err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return datasetManifest{}, err
	}
	if err := writeProjectMemoryFileAtomic(
		filepath.Join(outputRoot, "manifest.json"),
		append(raw, '\n'),
	); err != nil {
		return datasetManifest{}, err
	}
	return manifest, nil
}

func discoverDatasetTraces(inputs []string) ([]string, []string, error) {
	seen := make(map[string]bool)
	var traces, display []string
	for _, input := range inputs {
		absolute, err := filepath.Abs(input)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symlänkad datasetindata tillåts inte: %s", absolute)
		}
		display = append(display, filepath.Base(absolute))
		if info.Mode().IsRegular() {
			if filepath.Ext(absolute) != ".json" {
				return nil, nil, fmt.Errorf("datasetindata måste vara JSON: %s", absolute)
			}
			if !seen[absolute] {
				traces = append(traces, absolute)
				seen[absolute] = true
			}
			continue
		}
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("ogiltig datasetindata: %s", absolute)
		}
		err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == absolute {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return fmt.Errorf("symlänkad datasetindata tillåts inte: %s", path)
			}
			if entry.IsDir() || filepath.Ext(path) != ".json" {
				return nil
			}
			if !seen[path] {
				traces = append(traces, path)
				seen[path] = true
			}
			if len(traces) > maxDatasetTraces {
				return fmt.Errorf("fler än %d JSON-filer hittades", maxDatasetTraces)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(traces)
	sort.Strings(display)
	return traces, display, nil
}

func datasetExampleFromTrace(trace trainingTrace) (datasetExample, bool, error) {
	source, ok := latestDifferingDraft(trace.Drafts, trace.CorrectedFiles)
	if !ok || !hasRepairEvidence(trace) {
		return datasetExample{}, false, nil
	}
	if secret := detectDatasetSecret(datasetTraceContent(trace, source)); secret != "" {
		return datasetExample{}, false, fmt.Errorf("möjlig hemlighet upptäckt (%s)", secret)
	}
	payload := struct {
		Task        string             `json:"task"`
		Diagnostics string             `json:"verified_diagnostics"`
		Files       []workerResultFile `json:"current_files"`
	}{
		Task:        trace.Task,
		Diagnostics: trace.Diagnostics,
		Files:       source.Files,
	}
	userRaw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return datasetExample{}, false, err
	}
	targetRaw, err := json.Marshal(struct {
		Files []workerResultFile `json:"files"`
	}{Files: trace.CorrectedFiles})
	if err != nil {
		return datasetExample{}, false, err
	}
	combined := string(userRaw) + "\n" + string(targetRaw)
	id := evalContentHash(combined)
	return datasetExample{
		SchemaVersion: datasetSchemaVersion,
		ID:            id,
		SourceModel:   trace.Model,
		SourceJobID:   trace.JobID,
		Messages: []datasetMessage{
			{
				Role:    "system",
				Content: "Repair only the diagnosed source files. Return complete corrected files as JSON, preserve working APIs and do not return reasoning or Markdown.",
			},
			{Role: "user", Content: string(userRaw)},
			{Role: "assistant", Content: string(targetRaw)},
		},
	}, true, nil
}

func datasetTraceContent(trace trainingTrace, source workerResultDraft) string {
	var builder strings.Builder
	builder.WriteString(trace.Task)
	builder.WriteByte('\n')
	builder.WriteString(trace.Diagnostics)
	for _, file := range source.Files {
		builder.WriteByte('\n')
		builder.WriteString(file.Path)
		builder.WriteByte('\n')
		builder.WriteString(file.Content)
	}
	for _, file := range trace.CorrectedFiles {
		builder.WriteByte('\n')
		builder.WriteString(file.Path)
		builder.WriteByte('\n')
		builder.WriteString(file.Content)
	}
	return builder.String()
}

func latestDifferingDraft(
	drafts []workerResultDraft,
	corrected []workerResultFile,
) (workerResultDraft, bool) {
	correctedByPath := make(map[string]string, len(corrected))
	for _, file := range corrected {
		correctedByPath[file.Path] = file.Content
	}
	for index := len(drafts) - 1; index >= 0; index-- {
		draft := drafts[index]
		draftPaths := make(map[string]bool, len(draft.Files))
		different := false
		for _, file := range draft.Files {
			draftPaths[file.Path] = true
			if content, exists := correctedByPath[file.Path]; !exists || content != file.Content {
				different = true
				break
			}
		}
		if !different {
			for path := range correctedByPath {
				if !draftPaths[path] {
					different = true
					break
				}
			}
		}
		if different {
			return draft, true
		}
	}
	return workerResultDraft{}, false
}

func hasRepairEvidence(trace trainingTrace) bool {
	if len(trace.OriginalIssues) > 0 {
		return true
	}
	lower := strings.ToLower(trace.Diagnostics)
	return strings.Contains(lower, "failed") ||
		strings.Contains(lower, "undefined") ||
		strings.Contains(lower, "error:") ||
		strings.Contains(lower, "cannot ")
}

func detectDatasetSecret(content string) string {
	for _, candidate := range datasetSecretPatterns {
		if candidate.pattern.MatchString(content) {
			return candidate.name
		}
	}
	return ""
}

func datasetHoldout(id string, percent int) bool {
	if percent <= 0 || len(id) < 2 {
		return false
	}
	var firstByte int
	if _, err := fmt.Sscanf(id[:2], "%02x", &firstByte); err != nil {
		return false
	}
	return firstByte < percent*256/100
}

func writeDatasetJSONL(path string, examples []datasetExample) error {
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	for _, example := range examples {
		raw, err := json.Marshal(example)
		if err != nil {
			return err
		}
		if _, err := writer.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return writeProjectMemoryFileAtomic(path, output.Bytes())
}
