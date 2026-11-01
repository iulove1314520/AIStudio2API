package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func (s *server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	model, err := aistudio.QuotaModelID(r.URL.Query().Get("model"))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "A valid model ID is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	quota, err := s.config.Admin.AccountQuota(ctx, r.PathValue("id"), model)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}
