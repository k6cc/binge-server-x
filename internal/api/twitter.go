package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/binge-server/internal/configstore"
	"github.com/ordureconnoisseur/binge-server/internal/twitter"
)

// xFeedTTL — how long a fetched X media feed is reused before we re-hit
// X. On-demand fetches are human-paced already; this collapses rapid
// re-opens / pull-to-refresh of the same performer into one upstream
// call and keeps us well clear of rate limits.
const xFeedTTL = 15 * time.Minute

type xCacheEntry struct {
	media   []twitter.Media
	fetched time.Time
}

type xFeedCache struct {
	mu sync.Mutex
	m  map[string]xCacheEntry // keyed by handle (lowercased)
}

func newXFeedCache() *xFeedCache {
	return &xFeedCache{m: make(map[string]xCacheEntry)}
}

func (c *xFeedCache) get(handle string) ([]twitter.Media, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[handle]
	if !ok || time.Since(e.fetched) > xFeedTTL {
		return nil, false
	}
	return e.media, true
}

func (c *xFeedCache) put(handle string, media []twitter.Media) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[handle] = xCacheEntry{media: media, fetched: time.Now()}
}

// applyXCookies pushes the stored X cookies into the twitter client so
// rotation via /config takes effect without a restart (mirrors how the
// poller re-applies the Reddit cookie each tick). Cheap: the client
// no-ops when the values are unchanged.
func (s *Server) applyXCookies() {
	_ = s.twitter.SetCookies(
		s.store.Get(configstore.KeyXAuthToken),
		s.store.Get(configstore.KeyXCT0),
	)
}

type xFeedResponse struct {
	Handle string          `json:"handle"`
	Count  int             `json:"count"`
	Media  []twitter.Media `json:"media"`
}

// xFeed — GET /x/feed/{stashId}: resolve the performer's X handle from
// Stash (urls[]), then return their media tab.
func (s *Server) xFeed(w http.ResponseWriter, r *http.Request) {
	stashID := chi.URLParam(r, "stashId")
	if _, err := strconv.Atoi(stashID); err != nil {
		http.Error(w, "bad stashId", http.StatusBadRequest)
		return
	}

	// Resolve handle from Stash on demand. Apply current Stash creds first
	// (the poller normally owns the shared client; a request can race a
	// tick, so set them here too — SetCredentials is mutex-guarded).
	s.stash.SetCredentials(
		s.store.Get(configstore.KeyStashURL),
		s.store.Get(configstore.KeyStashAPIKey),
	)
	perf, err := s.stash.FetchPerformer(r.Context(), stashID)
	if err != nil {
		s.log.Warn("xFeed: resolve performer", "stashId", stashID, "err", err)
		http.Error(w, "performer lookup failed", http.StatusBadGateway)
		return
	}
	handle := twitter.HandleFromURLs(perf.URLs)
	if handle == "" {
		writeJSON(w, http.StatusOK, xFeedResponse{Handle: "", Count: 0, Media: []twitter.Media{}})
		return
	}
	s.serveXHandle(w, r, handle)
}

// xFeedByHandle — GET /x/handle/{handle}: fetch a handle directly,
// bypassing Stash resolution. Handy for testing and for clients that
// already know the handle.
func (s *Server) xFeedByHandle(w http.ResponseWriter, r *http.Request) {
	s.serveXHandle(w, r, chi.URLParam(r, "handle"))
}

func (s *Server) serveXHandle(w http.ResponseWriter, r *http.Request, handle string) {
	s.applyXCookies()
	if !s.twitter.HasCookies() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "x cookies not configured; set x_auth_token + x_ct0 via /config",
		})
		return
	}

	count := 40
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		count = v
	}

	key := handle // handle is case-insensitive on X; cache key as-is is fine for v1
	if media, ok := s.xCache.get(key); ok {
		writeJSON(w, http.StatusOK, xFeedResponse{Handle: handle, Count: len(media), Media: media})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	media, err := s.twitter.FetchMedia(ctx, handle, count)
	if err != nil {
		switch {
		case errors.Is(err, twitter.ErrBadHandle):
			http.Error(w, "invalid handle", http.StatusBadRequest)
		case errors.Is(err, twitter.ErrNoCookies):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "x cookies not configured"})
		default:
			s.log.Warn("xFeed: gallery-dl", "handle", handle, "err", err)
			http.Error(w, "x fetch failed", http.StatusBadGateway)
		}
		return
	}
	s.xCache.put(key, media)
	writeJSON(w, http.StatusOK, xFeedResponse{Handle: handle, Count: len(media), Media: media})
}
