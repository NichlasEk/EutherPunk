package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const (
	workerSchemaVersion  = 1
	defaultWorkerTimeout = 10 * time.Minute
	maxWorkerTimeout     = 30 * time.Minute
)

type workerOptions struct {
	Workspace      string
	Task           string
	Role           string
	Output         string
	Apply          bool
	VerifierDriven bool
	Timeout        time.Duration
}

type workerResult struct {
	SchemaVersion      int                    `json:"schema_version"`
	Status             string                 `json:"status"`
	Task               string                 `json:"task"`
	Role               string                 `json:"role"`
	Workspace          string                 `json:"workspace"`
	Applied            bool                   `json:"applied"`
	JobID              string                 `json:"job_id,omitempty"`
	Model              string                 `json:"model,omitempty"`
	Message            string                 `json:"message,omitempty"`
	CheckpointRevision int                    `json:"checkpoint_revision,omitempty"`
	InitialFiles       []workerResultFile     `json:"initial_files,omitempty"`
	Files              []workerResultFile     `json:"files,omitempty"`
	Drafts             []workerResultDraft    `json:"drafts,omitempty"`
	Issues             []string               `json:"issues,omitempty"`
	Activities         []workspaceJobActivity `json:"activities,omitempty"`
	Error              string                 `json:"error,omitempty"`
	StartedAt          string                 `json:"started_at"`
	FinishedAt         string                 `json:"finished_at"`
}

type workerResultFile struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

type workerResultDraft struct {
	Revision int                `json:"revision"`
	Files    []workerResultFile `json:"files"`
}

func runWorker(cfg *cliConfig, args []string, stdout, stderr io.Writer) error {
	options, err := parseWorkerOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	started := time.Now().UTC()
	result := workerResult{
		SchemaVersion: workerSchemaVersion,
		Status:        "failed",
		Task:          options.Task,
		Role:          options.Role,
		Applied:       false,
		StartedAt:     started.Format(time.RFC3339),
	}
	finish := func(workerErr error) error {
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		if workerErr != nil {
			result.Error = workerErr.Error()
		}
		if writeErr := writeWorkerResult(stdout, options.Output, result); writeErr != nil {
			if workerErr != nil {
				return fmt.Errorf("%v; skriv worker-resultat: %w", workerErr, writeErr)
			}
			return fmt.Errorf("skriv worker-resultat: %w", writeErr)
		}
		return workerErr
	}

	root, err := canonicalWorkspaceRoot(workspaceState{Root: options.Workspace})
	if err != nil {
		return finish(fmt.Errorf("arbetsyta: %w", err))
	}
	workspace := workspaceState{Root: root}
	result.Workspace = root
	cfg.workspace = workspace
	cfg.nonInteractiveAuth = true
	cfg.verifierDriven = options.VerifierDriven

	initialFiles, err := readWorkspaceFiles(workspace)
	if err != nil {
		return finish(fmt.Errorf("läs ursprunglig arbetsyta: %w", err))
	}
	result.InitialFiles = workerResultFiles(initialFiles)
	if err := cfg.ensureAuthenticated(false); err != nil {
		return finish(err)
	}
	if options.Apply {
		if err := ensureProjectMemory(workspace, options.Task); err != nil {
			return finish(fmt.Errorf("projektminne: %w", err))
		}
	}
	toolContext, err := workspaceContext(workspace)
	if err != nil {
		return finish(fmt.Errorf("läs arbetsyta: %w", err))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, options.Timeout)
	defer timeoutCancel()

	messages := []chatMessage{{
		Role:    "user",
		Content: workerTaskPrompt(options.Role, options.Task),
	}}
	job, status, err := startWorkspaceJob(ctx, *cfg, messages, toolContext)
	if err != nil {
		return finish(err)
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return finish(fmt.Errorf("starta worker-jobb: HTTP %d: %s", status, job.Error))
	}
	if job.ID == "" {
		return finish(errors.New("servern returnerade inget jobb-ID"))
	}
	result.JobID = job.ID
	result.Model = job.Model
	_, _ = fmt.Fprintf(stderr, "worker %s started with %s\n", shortWorkspaceJobID(job.ID), job.Model)

	if options.Apply {
		if err := recordProjectJobStarted(workspace, options.Task, job); err != nil {
			cancelWorkspaceJob(*cfg, job.ID)
			return finish(fmt.Errorf("projektminne: %w", err))
		}
	}

	currentJob := job
	lastActivity := 1
	lastDraftRevision := 0
	backedUpPaths := map[string]bool{}
	message, proposal, waitErr := waitWorkspaceJob(
		ctx,
		*cfg,
		job,
		stderr,
		&lastActivity,
		func(update workspaceJobResponse) error {
			currentJob = update
			if !options.Apply || update.DraftRev <= lastDraftRevision {
				return nil
			}
			if len(update.DraftFiles) == 0 {
				return errors.New("servern rapporterade en arbetskopierevision utan filer")
			}
			if err := applyWorkspaceDraft(
				workspace,
				update.DraftFiles,
				update.DraftRev,
				backedUpPaths,
				stderr,
			); err != nil {
				return err
			}
			lastDraftRevision = update.DraftRev
			result.Applied = true
			return nil
		},
	)
	result.JobID = currentJob.ID
	result.Model = currentJob.Model
	result.Message = message
	result.CheckpointRevision = currentJob.DraftRev
	result.Activities = append([]workspaceJobActivity(nil), currentJob.Activities...)
	result.Issues = projectReviewIssues(currentJob.Activities)
	resultFiles := proposal.Files
	if len(resultFiles) == 0 && currentJob.DraftRev > 0 {
		resultFiles = currentJob.DraftFiles
	}
	result.Files = workerResultFiles(resultFiles)
	result.Drafts = workerResultDrafts(currentJob.Drafts)

	switch {
	case waitErr != nil && (errors.Is(waitErr, errAgentInterrupted) || errors.Is(ctx.Err(), context.Canceled)):
		result.Status = "cancelled"
	case waitErr != nil:
		result.Status = "failed"
	case len(proposal.Files) > 0:
		result.Status = "completed"
	case currentJob.DraftRev > 0:
		result.Status = "needs_review"
	default:
		result.Status = "no_change"
	}
	if result.Status == "completed" || result.Status == "no_change" {
		result.Issues = nil
	}
	if options.Apply {
		memoryStatus := result.Status
		if memoryStatus == "completed" {
			memoryStatus = "accepted"
		}
		summary := result.Message
		if waitErr != nil {
			summary = waitErr.Error()
		}
		if memoryErr := recordProjectJobFinished(
			workspace,
			currentJob,
			memoryStatus,
			summary,
		); memoryErr != nil && waitErr == nil {
			waitErr = fmt.Errorf("uppdatera projektminne: %w", memoryErr)
			result.Status = "failed"
		}
	}
	return finish(waitErr)
}

func parseWorkerOptions(args []string, stderr io.Writer) (workerOptions, error) {
	options := workerOptions{
		Role:    "implementer",
		Timeout: defaultWorkerTimeout,
	}
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.Workspace, "workspace", "", "bounded workspace directory")
	flags.StringVar(&options.Task, "task", "", "bounded worker task")
	flags.StringVar(&options.Role, "role", options.Role, "worker role (implementer)")
	flags.StringVar(&options.Output, "output", "", "also write the JSON result to this file")
	flags.BoolVar(&options.Apply, "apply", false, "write draft checkpoints inside the workspace")
	flags.BoolVar(
		&options.VerifierDriven,
		"verifier-driven",
		false,
		"let an external executable verifier drive repair instead of model self-review",
	)
	flags.DurationVar(&options.Timeout, "timeout", options.Timeout, "job timeout (maximum 30m)")
	if err := flags.Parse(args); err != nil {
		return workerOptions{}, err
	}
	if strings.TrimSpace(options.Task) == "" && flags.NArg() > 0 {
		options.Task = strings.Join(flags.Args(), " ")
	} else if flags.NArg() > 0 {
		return workerOptions{}, errors.New("ange uppgiften antingen med --task eller som avslutande argument")
	}
	options.Workspace = strings.TrimSpace(options.Workspace)
	options.Task = strings.TrimSpace(options.Task)
	options.Role = strings.ToLower(strings.TrimSpace(options.Role))
	options.Output = strings.TrimSpace(options.Output)
	if options.Workspace == "" {
		return workerOptions{}, errors.New("--workspace krävs")
	}
	if options.Task == "" {
		return workerOptions{}, errors.New("--task krävs")
	}
	if options.Role != "implementer" {
		return workerOptions{}, fmt.Errorf("worker-rollen %q stöds inte ännu; använd implementer", options.Role)
	}
	if options.Timeout <= 0 || options.Timeout > maxWorkerTimeout {
		return workerOptions{}, errors.New("--timeout måste vara större än 0 och högst 30m")
	}
	return options, nil
}

func workerTaskPrompt(role, task string) string {
	return fmt.Sprintf(
		"WORKER ROLE: %s\nBOUNDED TASK:\n%s\n\nComplete only this task in the selected workspace. Preserve unrelated files.",
		role,
		task,
	)
}

func workerResultFiles(files []workspaceFile) []workerResultFile {
	out := make([]workerResultFile, 0, len(files))
	for _, file := range files {
		sum := sha256.Sum256([]byte(file.Content))
		out = append(out, workerResultFile{
			Path:    filepath.ToSlash(file.Path),
			Bytes:   len(file.Content),
			SHA256:  hex.EncodeToString(sum[:]),
			Content: file.Content,
		})
	}
	return out
}

func workerResultDrafts(drafts []workspaceJobDraft) []workerResultDraft {
	out := make([]workerResultDraft, 0, len(drafts))
	for _, draft := range drafts {
		out = append(out, workerResultDraft{
			Revision: draft.Revision,
			Files:    workerResultFiles(draft.Files),
		})
	}
	return out
}

func writeWorkerResult(stdout io.Writer, outputPath string, result workerResult) error {
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := stdout.Write(raw); err != nil {
		return err
	}
	if outputPath == "" || outputPath == "-" {
		return nil
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("osäker worker-resultatfil %q", absolute)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Stat(filepath.Dir(absolute)); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("resultatkatalogen är inte en katalog: %s", filepath.Dir(absolute))
	}
	return writeProjectMemoryFileAtomic(absolute, raw)
}
