package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zyperbot/internal/engine"
)

// GET /api/tasks
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.List())
}

// GET /api/tasks/{id} — full task config (for the edit form).
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	cfg, err := s.eng.GetConfig(id)
	if err != nil {
		writeErr(w, statusForTaskErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// POST /api/tasks {TaskConfig}
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var cfg engine.TaskConfig
	if err := decode(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if cfg.ContractAddress == "" && !cfg.HexMode {
		writeErr(w, http.StatusBadRequest, "contractAddress required")
		return
	}
	id, err := s.eng.Create(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

// PUT /api/tasks/{id} {TaskConfig}
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var cfg engine.TaskConfig
	if err := decode(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.eng.Update(id, cfg); err != nil {
		writeErr(w, statusForTaskErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// DELETE /api/tasks/{id}
func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.eng.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/tasks/{id}/start
func (s *Server) handleStartTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.eng.Start(id); err != nil {
		writeErr(w, statusForTaskErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/tasks/{id}/stop
func (s *Server) handleStopTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	s.eng.Stop(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/tasks/{id}/boost
func (s *Server) handleBoostTask(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.eng.Boost(id); err != nil {
		writeErr(w, statusForTaskErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/tasks/group/{group}/start
func (s *Server) handleStartGroup(w http.ResponseWriter, r *http.Request) {
	s.eng.StartGroup(chi.URLParam(r, "group"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/tasks/group/{group}/stop
func (s *Server) handleStopGroup(w http.ResponseWriter, r *http.Request) {
	s.eng.StopGroup(chi.URLParam(r, "group"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/logs — ring buffer snapshot for initial Logs page load.
func (s *Server) handleLogsSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.log.Snapshot())
}

func statusForTaskErr(err error) int {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrRunning):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
