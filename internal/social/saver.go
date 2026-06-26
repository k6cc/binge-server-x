// Package social implements "save a social post into Stash": download
// the media through binge-server's egress, place it under the configured
// library root as <root>/<source>/<handle>/, scan it into Stash, and
// write the metadata (source Studio + Tag + performer + url + date +
// caption).
//
// Everything is config-driven so it ships as a product: the write root
// (where this daemon writes) and the Stash root (the path Stash sees the
// same files at) are both runtime config; Studios/Tags are resolved by
// NAME (auto-created), never hardcoded ids. When the paths aren't
// configured the feature is simply unavailable (the API returns 503 and
// the client hides the button).
package social

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

// ErrNotConfigured — the social write/stash roots aren't set.
var ErrNotConfigured = errors.New("social save not configured (set the library paths)")

// sourceLabels maps an internal source key to its Studio/Tag display
// name. Generic defaults (not user-specific), safe to ship.
var sourceLabels = map[string]string{
	"x":         "X",
	"twitter":   "X",
	"reddit":    "Reddit",
	"redgifs":   "Redgifs",
	"instagram": "Instagram",
}

const socialParentTag = "Social Media"

type Saver struct {
	stash *stash.Client
	http  *http.Client

	mu        sync.RWMutex
	writeRoot string // path THIS daemon writes to (e.g. /library/social)
	stashRoot string // path Stash sees the same files at (e.g. Z:\Media\social)
}

func New(st *stash.Client) *Saver {
	return &Saver{stash: st, http: &http.Client{Timeout: 180 * time.Second}}
}

// SetPaths updates the write/stash roots (from env seed or POST /config).
func (s *Saver) SetPaths(writeRoot, stashRoot string) {
	s.mu.Lock()
	s.writeRoot = strings.TrimRight(writeRoot, `/\`)
	s.stashRoot = strings.TrimRight(stashRoot, `/\`)
	s.mu.Unlock()
}

func (s *Saver) roots() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeRoot, s.stashRoot
}

// Configured reports whether both roots are set (gates the feature).
func (s *Saver) Configured() bool {
	w, st := s.roots()
	return w != "" && st != ""
}

type SaveRequest struct {
	PerformerStashID string `json:"performerStashId"`
	Source           string `json:"source"` // x | reddit | redgifs | instagram
	Handle           string `json:"handle"` // optional; derived if empty
	ID               string `json:"id"`     // post/tweet id (used for the filename)
	MediaURL         string `json:"mediaUrl"`
	Kind             string `json:"kind"` // image | video
	SourceURL        string `json:"sourceUrl"`
	Text             string `json:"text"`
	CreatedUtc       int64  `json:"createdUtc"`
}

type SaveResult struct {
	StashType string `json:"stashType"` // scene | image
	StashID   string `json:"stashId"`
	Path      string `json:"path"`
	Handle    string `json:"handle"`
}

func (s *Saver) Save(ctx context.Context, req SaveRequest) (*SaveResult, error) {
	writeRoot, stashRoot := s.roots()
	if writeRoot == "" || stashRoot == "" {
		return nil, ErrNotConfigured
	}
	label := sourceLabels[strings.ToLower(req.Source)]
	if label == "" {
		return nil, fmt.Errorf("unknown source %q", req.Source)
	}
	if req.MediaURL == "" {
		return nil, errors.New("missing mediaUrl")
	}

	// Resolve the handle for the folder: explicit > from performer urls[]
	// > performer name. Best-effort performer fetch (also used for meta).
	perf, _ := s.stash.FetchPerformer(ctx, req.PerformerStashID)
	handle := req.Handle
	if handle == "" {
		handle = stash.PerformerHandle(perf, strings.ToLower(req.Source))
	}
	if handle == "" {
		handle = perf.Name
	}
	handle = stash.SanitizeSegment(handle)

	ext := extFromURL(req.MediaURL, req.Kind)
	base := stash.SanitizeSegment(req.ID)
	if base == "unknown" {
		base = stash.SanitizeSegment(strings.TrimSuffix(path.Base(urlPath(req.MediaURL)), "."+ext))
	}
	filename := base + "." + ext

	src := strings.ToLower(req.Source)
	writeDir := filepath.Join(writeRoot, src, handle)
	writePath := filepath.Join(writeDir, filename)

	// Build the Stash-side path with the separator implied by stashRoot
	// (Windows Stash = backslash, Linux = forward slash).
	sep := "/"
	if strings.Contains(stashRoot, `\`) {
		sep = `\`
	}
	stashDir := strings.Join([]string{stashRoot, src, handle}, sep)
	stashPath := stashDir + sep + filename

	if err := s.download(ctx, req, writeDir, writePath); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}

	if err := s.stash.MetadataScan(ctx, []string{stashDir}); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	// Poll until the scan has registered the file.
	stashID, stashType := "", ""
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		var err error
		if req.Kind == "video" {
			id, err = s.stash.FindSceneIDByPath(ctx, base, stashPath)
			stashType = "scene"
		} else {
			id, err = s.stash.FindImageIDByPath(ctx, base, stashPath)
			stashType = "image"
		}
		if err == nil && id != "" {
			stashID = id
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if stashID == "" {
		return nil, fmt.Errorf("file placed at %s but Stash hasn't registered it yet", stashPath)
	}

	studioID, err := s.stash.EnsureStudio(ctx, label)
	if err != nil {
		return nil, fmt.Errorf("ensure studio: %w", err)
	}
	tagID, err := s.stash.EnsureTag(ctx, label, socialParentTag)
	if err != nil {
		return nil, fmt.Errorf("ensure tag: %w", err)
	}

	meta := stash.EntityMeta{
		PerformerID: req.PerformerStashID,
		StudioID:    studioID,
		TagID:       tagID,
		URL:         req.SourceURL,
		Date:        dateFromUnix(req.CreatedUtc),
		Details:     req.Text,
	}
	if stashType == "scene" {
		err = s.stash.UpdateSceneMeta(ctx, stashID, meta)
	} else {
		err = s.stash.UpdateImageMeta(ctx, stashID, meta)
	}
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}

	return &SaveResult{StashType: stashType, StashID: stashID, Path: stashPath, Handle: handle}, nil
}

func (s *Saver) download(ctx context.Context, req SaveRequest, dir, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil // already downloaded
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, "GET", req.MediaURL, nil)
	if err != nil {
		return err
	}
	hr.Header.Set("User-Agent", "Mozilla/5.0 (compatible; binge-server)")
	// Some CDNs gate on Referer (same reason the reddit/redgifs proxies do).
	switch strings.ToLower(req.Source) {
	case "redgifs":
		hr.Header.Set("Referer", "https://www.redgifs.com/")
	case "reddit":
		hr.Header.Set("Referer", "https://www.reddit.com/")
	}
	resp, err := s.http.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}

// extFromURL guesses the file extension from the media URL (path ext or
// a ?format= param), falling back by kind.
func extFromURL(raw, kind string) string {
	p := urlPath(raw)
	if e := strings.TrimPrefix(strings.ToLower(path.Ext(p)), "."); e != "" && len(e) <= 4 {
		return e
	}
	if u, err := url.Parse(raw); err == nil {
		if f := u.Query().Get("format"); f != "" {
			return strings.ToLower(f)
		}
	}
	if kind == "video" {
		return "mp4"
	}
	return "jpg"
}

func urlPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

func dateFromUnix(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}
