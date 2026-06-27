package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/social"
)

// saveToStash — POST /save: download a social post's media through this
// daemon's egress, place it under the configured social library root,
// scan it into Stash, and tag it (source studio + tag + performer + url
// + date + caption). 503 when the library roots aren't configured.
func (s *Server) saveToStash(w http.ResponseWriter, r *http.Request) {
	// CSRF: same cross-origin guard as /config — this writes files + Stash.
	if !originAllowed(r.Header.Get("Origin"), s.allowedOrigins) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
		return
	}
	if s.saver == nil || !s.saver.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "save-to-Stash not configured; set the social library roots in config",
		})
		return
	}
	var req social.SaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	res, err := s.saver.Save(ctx, req)
	if err != nil {
		if errors.Is(err, social.ErrNotConfigured) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		s.log.Warn("save to stash failed", "source", req.Source, "performer", req.PerformerStashID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// Mark a saved PornHub video so the feed/stories stop surfacing it
	// immediately — independent of (and faster than) the background
	// tagger writing the scene's url. req.ID is the viewkey = the
	// pornhub_videos primary key. Best-effort.
	if strings.EqualFold(req.Source, "pornhub") && req.ID != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE pornhub_videos SET saved_at=datetime('now') WHERE video_id=?`,
			req.ID); err != nil {
			s.log.Warn("pornhub mark saved", "viewkey", req.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, res)
}
