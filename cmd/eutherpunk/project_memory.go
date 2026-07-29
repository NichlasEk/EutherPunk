package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	projectMemoryDirName   = ".eutherpunk"
	projectMemoryVersion   = 1
	maxProjectDocumentSize = 8 * 1024
	maxProjectStateSize    = 16 * 1024
	maxProjectJournalSize  = 64 * 1024
	maxProjectJournalTail  = 8 * 1024
)

type projectMemoryState struct {
	Version            int      `json:"version"`
	Project            string   `json:"project"`
	Status             string   `json:"status"`
	LastTask           string   `json:"last_task,omitempty"`
	Model              string   `json:"model,omitempty"`
	JobID              string   `json:"job_id,omitempty"`
	CheckpointRevision int      `json:"checkpoint_revision,omitempty"`
	Files              []string `json:"files,omitempty"`
	Issues             []string `json:"issues,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	UpdatedAt          string   `json:"updated_at"`
}

type projectJournalEvent struct {
	At       string   `json:"at"`
	Type     string   `json:"type"`
	Status   string   `json:"status,omitempty"`
	Task     string   `json:"task,omitempty"`
	Model    string   `json:"model,omitempty"`
	JobID    string   `json:"job_id,omitempty"`
	Revision int      `json:"revision,omitempty"`
	Files    []string `json:"files,omitempty"`
	Issues   []string `json:"issues,omitempty"`
	Message  string   `json:"message,omitempty"`
}

func ensureProjectMemory(workspace workspaceState, firstTask string) error {
	root, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return err
	}
	if err := ensurePrivateProjectMemoryDir(dir); err != nil {
		return err
	}
	projectPath := filepath.Join(dir, "project.md")
	if exists, err := safeProjectMemoryFileExists(projectPath); err != nil {
		return err
	} else if !exists {
		project := fmt.Sprintf(`# EutherPunk Project

## Project

- Name: %s
- Root: %s

## Goal

%s

## Durable rules

- Continue from files and verified checkpoints; do not restart from a blank design.
- Preserve working behavior while repairing concrete failures.
- Treat .eutherpunk/state.json as harness-owned runtime truth.

## Commands

- Add project-specific run and test commands here when they are known.
`, filepath.Base(root), filepath.Base(root), compactProjectText(firstTask, 1000))
		if err := writeProjectMemoryFileAtomic(projectPath, []byte(project)); err != nil {
			return err
		}
	}
	statePath := filepath.Join(dir, "state.json")
	if exists, err := safeProjectMemoryFileExists(statePath); err != nil {
		return err
	} else if !exists {
		state := projectMemoryState{
			Version:   projectMemoryVersion,
			Project:   filepath.Base(root),
			Status:    "ready",
			LastTask:  compactProjectText(firstTask, 1000),
			Summary:   "Project memory initialized.",
			UpdatedAt: projectMemoryTimestamp(),
		}
		if err := saveProjectMemoryState(dir, state); err != nil {
			return err
		}
		return appendProjectJournal(dir, projectJournalEvent{
			Type:    "project_initialized",
			Status:  "ready",
			Task:    state.LastTask,
			Message: state.Summary,
		})
	}
	return nil
}

func recordProjectJobStarted(
	workspace workspaceState,
	task string,
	job workspaceJobResponse,
) error {
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return err
	}
	state, err := loadProjectMemoryState(dir)
	if err != nil {
		return err
	}
	state.Status = "working"
	state.LastTask = compactProjectText(task, 2000)
	state.Model = compactProjectText(job.Model, 200)
	state.JobID = compactProjectText(job.ID, 100)
	state.CheckpointRevision = 0
	state.Files = nil
	state.Issues = nil
	state.Summary = "Workspace job started."
	state.UpdatedAt = projectMemoryTimestamp()
	if err := saveProjectMemoryState(dir, state); err != nil {
		return err
	}
	return appendProjectJournal(dir, projectJournalEvent{
		Type:    "job_started",
		Status:  state.Status,
		Task:    state.LastTask,
		Model:   state.Model,
		JobID:   state.JobID,
		Message: state.Summary,
	})
}

func recordProjectCheckpoint(
	workspace workspaceState,
	revision int,
	files []workspaceFile,
) error {
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return err
	}
	state, err := loadProjectMemoryState(dir)
	if err != nil {
		return err
	}
	paths := projectFilePaths(files)
	state.Status = "iterating"
	state.CheckpointRevision = revision
	state.Files = paths
	state.Summary = fmt.Sprintf("Checkpoint %d written; validation and repair continue.", revision)
	state.UpdatedAt = projectMemoryTimestamp()
	if err := saveProjectMemoryState(dir, state); err != nil {
		return err
	}
	return appendProjectJournal(dir, projectJournalEvent{
		Type:     "checkpoint_written",
		Status:   state.Status,
		Task:     state.LastTask,
		Model:    state.Model,
		JobID:    state.JobID,
		Revision: revision,
		Files:    paths,
		Message:  state.Summary,
	})
}

func recordProjectJobFinished(
	workspace workspaceState,
	job workspaceJobResponse,
	status, summary string,
) error {
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return err
	}
	state, err := loadProjectMemoryState(dir)
	if err != nil {
		return err
	}
	state.Status = status
	state.Model = compactProjectText(job.Model, 200)
	state.JobID = compactProjectText(job.ID, 100)
	state.CheckpointRevision = job.DraftRev
	if len(job.Files) > 0 {
		state.Files = projectFilePaths(job.Files)
	} else if len(job.DraftFiles) > 0 {
		state.Files = projectFilePaths(job.DraftFiles)
	}
	if status == "accepted" || status == "no_change" {
		state.Issues = nil
	} else {
		state.Issues = projectReviewIssues(job.Activities)
	}
	state.Summary = compactProjectText(summary, 2000)
	state.UpdatedAt = projectMemoryTimestamp()
	if err := saveProjectMemoryState(dir, state); err != nil {
		return err
	}
	return appendProjectJournal(dir, projectJournalEvent{
		Type:     "job_finished",
		Status:   state.Status,
		Task:     state.LastTask,
		Model:    state.Model,
		JobID:    state.JobID,
		Revision: state.CheckpointRevision,
		Files:    state.Files,
		Issues:   state.Issues,
		Message:  state.Summary,
	})
}

func projectMemoryContext(workspace workspaceState) (string, error) {
	_, dir, err := projectMemoryPaths(workspace)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	project, err := readBoundedProjectMemoryFile(filepath.Join(dir, "project.md"), maxProjectDocumentSize)
	if err != nil {
		return "", err
	}
	state, err := readBoundedProjectMemoryFile(filepath.Join(dir, "state.json"), maxProjectStateSize)
	if err != nil {
		return "", err
	}
	journal, err := readProjectJournalTail(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("PROJEKTSPECIFIKT LÅNGTIDSMINNE (HARNESS-ÄGT)\n")
	out.WriteString("Använd detta för kontinuitet. Projektfiler och nya verifieringar har företräde vid konflikt.\n")
	if project != "" {
		out.WriteString("\n--- .eutherpunk/project.md ---\n")
		out.WriteString(project)
		out.WriteByte('\n')
	}
	if state != "" {
		out.WriteString("\n--- .eutherpunk/state.json ---\n")
		out.WriteString(state)
		out.WriteByte('\n')
	}
	if journal != "" {
		out.WriteString("\n--- senaste journalhändelser ---\n")
		out.WriteString(journal)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

func projectMemoryPaths(workspace workspaceState) (string, string, error) {
	root, err := canonicalWorkspaceRoot(workspace)
	if err != nil {
		return "", "", err
	}
	return root, filepath.Join(root, projectMemoryDirName), nil
}

func ensurePrivateProjectMemoryDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("osäker projektminneskatalog %q", dir)
	}
	return nil
}

func loadProjectMemoryState(dir string) (projectMemoryState, error) {
	path := filepath.Join(dir, "state.json")
	if _, err := safeProjectMemoryFileExists(path); err != nil {
		return projectMemoryState{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return projectMemoryState{}, err
	}
	if len(raw) > maxProjectStateSize {
		return projectMemoryState{}, errors.New("projektets state.json är för stor")
	}
	var state projectMemoryState
	if err := json.Unmarshal(raw, &state); err != nil {
		return projectMemoryState{}, err
	}
	if state.Version != projectMemoryVersion {
		return projectMemoryState{}, fmt.Errorf("projektminnesversion %d stöds inte", state.Version)
	}
	return state, nil
}

func saveProjectMemoryState(dir string, state projectMemoryState) error {
	state.Version = projectMemoryVersion
	if state.Project == "" {
		state.Project = filepath.Base(filepath.Dir(dir))
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maxProjectStateSize {
		return errors.New("projektets state.json blev för stor")
	}
	return writeProjectMemoryFileAtomic(filepath.Join(dir, "state.json"), raw)
}

func writeProjectMemoryFileAtomic(path string, raw []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.new")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func appendProjectJournal(dir string, event projectJournalEvent) error {
	event.At = projectMemoryTimestamp()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "journal.jsonl")
	exists, err := safeProjectMemoryFileExists(path)
	if err != nil {
		return err
	}
	var journal []byte
	if exists {
		journal, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	journal = append(journal, raw...)
	journal = append(journal, '\n')
	if len(journal) > maxProjectJournalSize {
		journal = journal[len(journal)-maxProjectJournalSize/2:]
		if index := bytes.IndexByte(journal, '\n'); index >= 0 {
			journal = journal[index+1:]
		}
	}
	return writeProjectMemoryFileAtomic(path, journal)
}

func readProjectJournalTail(path string) (string, error) {
	tail, err := readFileTail(path, maxProjectJournalTail)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if index := strings.IndexByte(tail, '\n'); index >= 0 {
		tail = tail[index+1:]
	}
	return strings.TrimSpace(tail), nil
}

func readFileTail(path string, limit int64) (string, error) {
	if _, err := safeProjectMemoryFileExists(path); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(bufio.NewReader(file), limit))
	return string(raw), err
}

func readBoundedProjectMemoryFile(path string, limit int) (string, error) {
	exists, err := safeProjectMemoryFileExists(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > limit {
		return "", fmt.Errorf("%s är större än %d byte", filepath.Base(path), limit)
	}
	return strings.TrimSpace(string(raw)), nil
}

func safeProjectMemoryFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("osäker projektminnesfil %q", path)
	}
	return true, nil
}

func projectReviewIssues(activities []workspaceJobActivity) []string {
	issues := make([]string, 0, 8)
	for _, activity := range activities {
		const prefix = "Granskaren hittade:"
		if !strings.HasPrefix(activity.Message, prefix) {
			continue
		}
		issue := compactProjectText(strings.TrimSpace(strings.TrimPrefix(activity.Message, prefix)), 500)
		if issue != "" {
			issues = append(issues, issue)
		}
		if len(issues) == 8 {
			break
		}
	}
	return issues
}

func projectFilePaths(files []workspaceFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.ToSlash(strings.TrimSpace(file.Path))
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func compactProjectText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) > limit {
		text = strings.TrimSpace(text[:limit])
	}
	return text
}

func projectMemoryTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}
