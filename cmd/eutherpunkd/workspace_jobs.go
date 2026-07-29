package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID         string                  `json:"id"`
	Status     string                  `json:"status"`
	Model      string                  `json:"model,omitempty"`
	Message    string                  `json:"message,omitempty"`
	Activities []workspaceJobActivity  `json:"activities,omitempty"`
	Files      []workspaceResponseFile `json:"files,omitempty"`
	DraftFiles []workspaceResponseFile `json:"draft_files,omitempty"`
	DraftRev   int                     `json:"draft_revision,omitempty"`
	Drafts     []workspaceJobDraft     `json:"drafts,omitempty"`
	Error      string                  `json:"error,omitempty"`
	CreatedAt  time.Time               `json:"created_at"`
	UpdatedAt  time.Time               `json:"updated_at"`

	user         string
	task         string
	cancel       context.CancelFunc
	localRepairs int
}

type workspaceJobActivity struct {
	Sequence int       `json:"sequence"`
	Message  string    `json:"message"`
	At       time.Time `json:"at"`
}

type workspaceJobDraft struct {
	Revision int                     `json:"revision"`
	Files    []workspaceResponseFile `json:"files"`
}

var (
	workspaceJobsMu sync.Mutex
	workspaceJobs   = map[string]*workspaceJob{}
)

func handleWorkspaceJobStart(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || (principal.AuthMode != "cli_token" && principal.AuthMode != "disabled") {
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
		model := workspaceModelForConfig(cfg)
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
			ID:      randomID(),
			Status:  "queued",
			Model:   model,
			Message: "Kodjobbet köades.",
			Activities: []workspaceJobActivity{{
				Sequence: 1,
				Message:  "Kodjobbet har placerats i kön.",
				At:       now,
			}},
			CreatedAt: now,
			UpdatedAt: now,
			user:      principal.User,
			task:      lastUserMessage(messages),
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

func workspaceModelForConfig(cfg serverConfig) string {
	if model := strings.TrimSpace(cfg.workspaceModel); model != "" {
		return model
	}
	return strings.TrimSpace(cfg.model)
}

func runWorkspaceJob(
	ctx context.Context,
	cfg serverConfig,
	jobID, model, system string,
	messages []ollamaMessage,
) {
	task := lastUserMessage(messages)
	updateWorkspaceJob(jobID, func(job *workspaceJob) {
		job.Status = "running"
		job.Message = "Modellen arbetar med filförslaget."
		appendWorkspaceJobActivityLocked(job, "Arbetsytekontext och instruktioner har förberetts.")
	})
	progress := func(message string) {
		updateWorkspaceJob(jobID, func(job *workspaceJob) {
			if job.Status == "running" {
				appendWorkspaceJobActivityLocked(job, message)
			}
		})
	}
	message, files, err := askWorkspaceOllama(
		ctx,
		cfg.ollamaURL,
		model,
		system,
		messages,
		progress,
	)
	if err == nil && len(files) > 0 {
		publishWorkspaceDraft(jobID, files)
		message, files, err = qualityReviewWorkspaceProposal(
			ctx, cfg, model, task, message, files, progress,
			func(files []workspaceResponseFile) {
				publishWorkspaceDraft(jobID, files)
			},
		)
	}
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
			totalBytes := 0
			for _, file := range files {
				totalBytes += len(file.Content)
			}
			appendWorkspaceJobActivityLocked(
				job,
				fmt.Sprintf("Filförslaget är klart: %d fil(er), %d byte.", len(files), totalBytes),
			)
		}
	})
}

func qualityReviewWorkspaceProposal(
	ctx context.Context,
	cfg serverConfig,
	model, task, message string,
	files []workspaceResponseFile,
	progress func(string),
	publishDraft func([]workspaceResponseFile),
) (string, []workspaceResponseFile, error) {
	for round := 0; round <= maxWorkspaceRepairRounds; round++ {
		progress(fmt.Sprintf("Kvalitetsgranskning %d kontrollerar logik och krav.", round+1))
		review, err := reviewWorkspaceProposalOllama(
			ctx,
			cfg.ollamaURL,
			model,
			task,
			message,
			files,
		)
		if err != nil {
			return "", nil, fmt.Errorf("kvalitetsgranskning: %w", err)
		}
		if review.Accepted {
			progress(fmt.Sprintf("Kvalitetsgranskning %d godkände förslaget.", round+1))
			return message, files, nil
		}
		for _, issue := range review.Issues {
			progress("Granskaren hittade: " + issue)
		}
		if round == maxWorkspaceRepairRounds {
			progress("Förslaget stoppades efter två misslyckade reparationsvarv.")
			return "Slutförslaget klarade inte kvalitetsgranskningen efter två reparationsvarv. I AUTO-läge behålls den sista arbetskopian för fortsatt arbete.", nil, nil
		}
		progress(fmt.Sprintf("Reparationsvarv %d startar.", round+1))
		message, files, err = repairWorkspaceProposalOllama(
			ctx,
			cfg.ollamaURL,
			model,
			task,
			message,
			files,
			review.Issues,
			progress,
		)
		if err != nil {
			return "", nil, err
		}
		if publishDraft != nil && len(files) > 0 {
			publishDraft(files)
		}
	}
	return message, files, nil
}

func publishWorkspaceDraft(jobID string, files []workspaceResponseFile) {
	updateWorkspaceJob(jobID, func(job *workspaceJob) {
		if job.Status != "running" || len(files) == 0 {
			return
		}
		job.DraftRev++
		job.DraftFiles = append([]workspaceResponseFile(nil), files...)
		job.Drafts = append(job.Drafts, workspaceJobDraft{
			Revision: job.DraftRev,
			Files:    append([]workspaceResponseFile(nil), files...),
		})
		appendWorkspaceJobActivityLocked(
			job,
			fmt.Sprintf("Arbetskopierevision %d är klar för lokal skrivning.", job.DraftRev),
		)
	})
}

type workspaceRepairRequest struct {
	Diagnostics string `json:"diagnostics"`
}

func handleWorkspaceJobRepair(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.AuthMode != "cli_token" {
			writeError(w, http.StatusForbidden, errors.New("workspace repairs require CLI authentication"))
			return
		}
		var req workspaceRepairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Diagnostics = strings.TrimSpace(req.Diagnostics)
		if req.Diagnostics == "" || len(req.Diagnostics) > 16*1024 {
			writeError(w, http.StatusBadRequest, errors.New("valid local diagnostics are required"))
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
		if job.Status != "completed" || len(job.Files) == 0 {
			workspaceJobsMu.Unlock()
			writeError(w, http.StatusConflict, errors.New("only a completed proposal can be repaired"))
			return
		}
		if job.localRepairs >= 2 {
			workspaceJobsMu.Unlock()
			writeError(w, http.StatusConflict, errors.New("local repair limit reached"))
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		job.localRepairs++
		job.Status = "running"
		job.Error = ""
		job.cancel = cancel
		job.UpdatedAt = time.Now().UTC()
		appendWorkspaceJobActivityLocked(job, "Den lokala körkontrollen hittade ett fel.")
		appendWorkspaceJobActivityLocked(job, "Diagnosen skickas till modellen för reparation.")
		model := job.Model
		task := job.task
		message := job.Message
		files := append([]workspaceResponseFile(nil), job.Files...)
		view := workspaceJobViewLocked(job)
		workspaceJobsMu.Unlock()

		go runWorkspaceJobLocalRepair(
			ctx, cfg, id, model, task, message, files, req.Diagnostics,
		)
		writeJSON(w, http.StatusAccepted, view)
	}
}

func runWorkspaceJobLocalRepair(
	ctx context.Context,
	cfg serverConfig,
	jobID, model, task, message string,
	files []workspaceResponseFile,
	diagnostics string,
) {
	progress := func(message string) {
		updateWorkspaceJob(jobID, func(job *workspaceJob) {
			if job.Status == "running" {
				appendWorkspaceJobActivityLocked(job, message)
			}
		})
	}
	message, files, err := repairWorkspaceProposalOllama(
		ctx,
		cfg.ollamaURL,
		model,
		task,
		message,
		files,
		[]string{"Lokal körkontroll: " + diagnostics},
		progress,
	)
	if err == nil && len(files) > 0 {
		publishWorkspaceDraft(jobID, files)
		message, files, err = qualityReviewWorkspaceProposal(
			ctx, cfg, model, task, message, files, progress,
			func(files []workspaceResponseFile) {
				publishWorkspaceDraft(jobID, files)
			},
		)
	}
	updateWorkspaceJob(jobID, func(job *workspaceJob) {
		if job.Status == "cancelled" {
			return
		}
		if err != nil {
			job.Status = "failed"
			job.Message = "Reparationsjobbet misslyckades."
			job.Error = err.Error()
			return
		}
		job.Status = "completed"
		job.Message = message
		job.Files = files
		totalBytes := 0
		for _, file := range files {
			totalBytes += len(file.Content)
		}
		appendWorkspaceJobActivityLocked(
			job,
			fmt.Sprintf("Det reparerade filförslaget är klart: %d fil(er), %d byte.", len(files), totalBytes),
		)
	})
}

func appendWorkspaceJobActivityLocked(job *workspaceJob, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if count := len(job.Activities); count > 0 && job.Activities[count-1].Message == message {
		return
	}
	sequence := 1
	if count := len(job.Activities); count > 0 {
		sequence = job.Activities[count-1].Sequence + 1
	}
	job.Activities = append(job.Activities, workspaceJobActivity{
		Sequence: sequence,
		Message:  message,
		At:       time.Now().UTC(),
	})
	if len(job.Activities) > 32 {
		job.Activities = append([]workspaceJobActivity(nil), job.Activities[len(job.Activities)-32:]...)
	}
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
	view.task = ""
	view.cancel = nil
	view.Files = append([]workspaceResponseFile(nil), job.Files...)
	view.DraftFiles = append([]workspaceResponseFile(nil), job.DraftFiles...)
	view.Drafts = make([]workspaceJobDraft, len(job.Drafts))
	for i, draft := range job.Drafts {
		view.Drafts[i] = workspaceJobDraft{
			Revision: draft.Revision,
			Files:    append([]workspaceResponseFile(nil), draft.Files...),
		}
	}
	view.Activities = append([]workspaceJobActivity(nil), job.Activities...)
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
