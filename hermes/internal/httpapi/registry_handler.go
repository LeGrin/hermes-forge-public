package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/projectstore"
)

// activeSession represents a single active session within a project.
type activeSession struct {
	EnvelopeID string    `json:"envelope_id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	Executor   string    `json:"executor"`
	StartedAt  time.Time `json:"started_at"`
}

// enrichedProject extends projectstore.Project with active_sessions.
type enrichedProject struct {
	Project        string          `json:"project"`
	Domain         string          `json:"domain"`
	TargetNode     string          `json:"target_node"`
	TargetExecutor string          `json:"target_executor"`
	WorkingDir     string          `json:"working_dir"`
	CreatedAt      string          `json:"created_at"`
	IconPath       string          `json:"icon_path"`
	ActiveSessions []activeSession `json:"active_sessions"`
}

type registryHandler struct {
	store     *projectstore.Store
	envelopes *envelopestore.Store
	logger    *slog.Logger
}

func (h *registryHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /registry/projects", h.list)
	mux.HandleFunc("POST /registry/projects", h.create)
	mux.HandleFunc("PATCH /registry/projects/{name}/icon", h.patchIcon)
}

func (h *registryHandler) list(w http.ResponseWriter, r *http.Request) {
	projects, err := h.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		h.logger.Error("registry list failed", "err", err)
		return
	}

	if h.envelopes == nil {
		writeJSON(w, http.StatusOK, projects)
		return
	}

	enriched, err := h.enrichWithActiveSessions(r.Context(), projects)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		h.logger.Error("list active sessions failed", "err", err)
		return
	}
	writeJSON(w, http.StatusOK, enriched)
}

func (h *registryHandler) enrichWithActiveSessions(ctx context.Context, projects []*projectstore.Project) ([]enrichedProject, error) {
	projectNames := make([]string, len(projects))
	for i, p := range projects {
		projectNames[i] = p.Project
	}

	sessionsByProject, err := h.envelopes.ListActiveSessionsForProjects(ctx, projectNames)
	if err != nil {
		return nil, err
	}

	enriched := make([]enrichedProject, len(projects))
	for i, p := range projects {
		enriched[i] = h.buildEnrichedProject(p, sessionsByProject[p.Project])
	}
	return enriched, nil
}

func (h *registryHandler) buildEnrichedProject(p *projectstore.Project, sessions []envelopestore.ActiveSession) enrichedProject {
	activeSessions := make([]activeSession, len(sessions))
	for j, s := range sessions {
		startedAt := s.CreatedAt
		if s.StartedAt != nil {
			startedAt = *s.StartedAt
		}
		activeSessions[j] = activeSession{
			EnvelopeID: s.EnvelopeID,
			Title:      s.Title,
			Status:     string(s.Status),
			Executor:   s.Executor,
			StartedAt:  startedAt,
		}
	}
	if activeSessions == nil {
		activeSessions = []activeSession{}
	}
	return enrichedProject{
		Project:        p.Project,
		Domain:         p.Domain,
		TargetNode:     p.TargetNode,
		TargetExecutor: p.TargetExecutor,
		WorkingDir:     p.WorkingDir,
		CreatedAt:      p.CreatedAt,
		IconPath:       p.IconPath,
		ActiveSessions: activeSessions,
	}
}

func (h *registryHandler) create(w http.ResponseWriter, r *http.Request) {
	var p projectstore.Project
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if p.Project == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_project", "project is required")
		return
	}
	if p.WorkingDir == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_working_dir", "working_dir is required")
		return
	}
	if !filepath.IsAbs(p.WorkingDir) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_working_dir", "working_dir must be an absolute path")
		return
	}
	p.WorkingDir = filepath.Clean(p.WorkingDir)
	if p.Domain == "" {
		p.Domain = "default"
	}
	if p.TargetNode == "" {
		p.TargetNode = "mac-forge"
	}
	if p.TargetExecutor == "" {
		p.TargetExecutor = "claude"
	}

	if err := h.store.Insert(r.Context(), &p); errors.Is(err, projectstore.ErrDuplicate) {
		writeError(w, http.StatusConflict, "duplicate_project", "project already registered")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		h.logger.Error("registry insert failed", "err", err)
		return
	}

	stored, err := h.store.Get(r.Context(), p.Project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		h.logger.Error("registry get after insert failed", "err", err)
		return
	}
	writeJSON(w, http.StatusCreated, stored)
}

func (h *registryHandler) patchIcon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_name", "project name is required")
		return
	}

	var req struct {
		IconPath string `json:"icon_path"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.IconPath == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_icon_path", "icon_path is required")
		return
	}

	if err := h.store.SetIconPath(r.Context(), name, req.IconPath); errors.Is(err, projectstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project_not_found", "project does not exist")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		h.logger.Error("set icon_path failed", "project", name, "err", err)
		return
	}

	updated, err := h.store.Get(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
