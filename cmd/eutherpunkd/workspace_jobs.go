package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	workspaceJobRetention         = 30 * time.Minute
	maxActiveWorkspaceJobsPerUser = 2
)

type workspaceJob struct {
	ID        string                  `json:"id"`
	Status    string                  `json:"status"`
	Model     string                  `json:"model,omitempty"`
	Message   string                  `json:"message,omitempty"`
	Files     []workspaceResponseFile `json:"files,omitempty"`
	Error     string                  `json:"error,omitempty"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`

	user   string
	cancel context.CancelFunc
}

var (
	workspaceJobsMu sync.Mutex
	workspaceJobs   = map[string]*workspaceJob{}
)

func handleWorkspaceJobStart(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.AuthMode != "cli_token" {
			writeError(w, http.StatusForbidden, errors.New("workspace jobs require CLI authentication"))
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !req.LocalWorkspace {
			writeError(w, http.StatusBadRequest, errors.New("local_workspace is required"))
			return
		}
		messages := requestMessages(req)
		if len(messages) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("message is required"))
			return
		}
		messages = messagesWithClientContext(r, req.ClientContext, messages)
		settings, err := readUserSettings(cfg, requestUser(r, cfg))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		prompts, _, err := readPromptSettings(cfg.promptsPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		model := selectedChatModel(settings, req.Model, messages)
		if isVisionRequest(settings, model, messages) {
			writeError(w, http.StatusBadRequest, errors.New("workspace jobs do not accept image requests"))
			return
		}
		messages = messagesForSelectedModel(settings, model, messages)
		system := strings.TrimSpace(req.System)
		if system == "" {
			system = systemPromptForMessages(prompts, messages)
		}
		system = systemPromptWithImageTool(system, prompts)

		now := time.Now().UTC()
		ctx, cancel := context.WithCancel(context.Background())
		job := &workspaceJob{
			ID:        randomID(),
			Status:    "queued",
			Model:     model,
			Message:   "Kodjobbet köades.",
			CreatedAt: now,
			UpdatedAt: now,
			user:      principal.User,
			cancel:    cancel,
		}
		workspaceJobsMu.Lock()
		cleanupWorkspaceJobsLocked(now)
		active := 0
		for _, existing := range workspaceJobs {
			if existing.user == principal.User &&
				(existing.Status == "queued" || existing.Status == "running") {
				active++
			}
		}
		if active >= maxActiveWorkspaceJobsPerUser {
			workspaceJobsMu.Unlock()
			cancel()
			writeError(w, http.StatusTooManyRequests, errors.New("too many active workspace jobs"))
			return
		}
		workspaceJobs[job.ID] = job
		workspaceJobsMu.Unlock()

		go runWorkspaceJob(ctx, cfg, job.ID, model, system, messages)
		writeJSON(w, http.StatusAccepted, workspaceJobView(job))
	}
}

func runWorkspaceJob(
	ctx context.Context,
	cfg serverConfig,
	jobID, model, system string,
	messages []ollamaMessage,
) {
	updateWorkspaceJob(jobID, func(job *workspaceJob) {
		job.Status = "running"
		job.Message = "Modellen arbetar med filförslaget."
	})
	message, files, err := askWorkspaceOllama(ctx, cfg.ollamaURL, model, system, messages)
	updateWorkspaceJob(jobID, func(job *workspaceJob) {
		if job.Status == "cancelled" {
			return
		}
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			job.Status = "cancelled"
			job.Message = "Kodjobbet avbröts."
		case err != nil:
			job.Status = "failed"
			job.Error = err.Error()
			job.Message = "Kodjobbet misslyckades."
			log.Printf("workspace job %s failed: %v", jobID, err)
		default:
			job.Status = "completed"
			job.Message = message
			job.Files = files
		}
	})
}

func handleWorkspaceJobGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			http.NotFound(w, r)
			return
		}
		id := safeID(r.PathValue("id"))
		workspaceJobsMu.Lock()
		job, ok := workspaceJobs[id]
		if !ok || job.user != principal.User {
			workspaceJobsMu.Unlock()
			http.NotFound(w, r)
			return
		}
		view := workspaceJobViewLocked(job)
		workspaceJobsMu.Unlock()
		writeJSON(w, http.StatusOK, view)
	}
}

func handleWorkspaceJobCancel() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			http.NotFound(w, r)
			return
		}
		id := safeID(r.PathValue("id"))
		workspaceJobsMu.Lock()
		job, ok := workspaceJobs[id]
		if !ok || job.user != principal.User {
			workspaceJobsMu.Unlock()
			http.NotFound(w, r)
			return
		}
		if job.Status == "queued" || job.Status == "running" {
			job.Status = "cancelled"
			job.Message = "Kodjobbet avbröts."
			job.UpdatedAt = time.Now().UTC()
			job.cancel()
		}
		view := workspaceJobViewLocked(job)
		workspaceJobsMu.Unlock()
		writeJSON(w, http.StatusOK, view)
	}
}

func updateWorkspaceJob(id string, update func(*workspaceJob)) {
	workspaceJobsMu.Lock()
	defer workspaceJobsMu.Unlock()
	job, ok := workspaceJobs[id]
	if !ok {
		return
	}
	update(job)
	job.UpdatedAt = time.Now().UTC()
}

func workspaceJobView(job *workspaceJob) workspaceJob {
	workspaceJobsMu.Lock()
	defer workspaceJobsMu.Unlock()
	return workspaceJobViewLocked(job)
}

func workspaceJobViewLocked(job *workspaceJob) workspaceJob {
	view := *job
	view.user = ""
	view.cancel = nil
	view.Files = append([]workspaceResponseFile(nil), job.Files...)
	return view
}

func cleanupWorkspaceJobsLocked(now time.Time) {
	for id, job := range workspaceJobs {
		if (job.Status == "completed" || job.Status == "failed" || job.Status == "cancelled") &&
			now.Sub(job.UpdatedAt) > workspaceJobRetention {
			delete(workspaceJobs, id)
		}
	}
}
