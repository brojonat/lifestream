package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TypeArtifactSaved is a Claude artifact the owner kept: uploaded HTML, a
// claude.ai link, or both (VISION.md "Claude artifacts as textbooks").
const TypeArtifactSaved = "artifact.saved"

// ArtifactSaved is the payload. Title is required, and at least one of
// HTML and URL; URL must be an https link on claude.ai.
type ArtifactSaved struct {
	Title string `json:"title"`
	HTML  string `json:"html,omitempty"`
	URL   string `json:"url,omitempty"`
}

// ParseArtifact validates an artifact.saved payload.
func ParseArtifact(payload json.RawMessage) (ArtifactSaved, error) {
	var a ArtifactSaved
	if err := json.Unmarshal(payload, &a); err != nil {
		return ArtifactSaved{}, invalid("%s payload: %v", TypeArtifactSaved, err)
	}
	if strings.TrimSpace(a.Title) == "" {
		return ArtifactSaved{}, invalid("%s needs a title", TypeArtifactSaved)
	}
	if strings.TrimSpace(a.HTML) == "" && a.URL == "" {
		return ArtifactSaved{}, invalid("%s needs html or a claude.ai url", TypeArtifactSaved)
	}
	if a.URL != "" {
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || (u.Host != "claude.ai" && !strings.HasSuffix(u.Host, ".claude.ai")) || u.User != nil {
			return ArtifactSaved{}, invalid("%s url must be an https link on claude.ai, got %q", TypeArtifactSaved, a.URL)
		}
	}
	return a, nil
}

// Artifact is a saved artifact as lists show it: no HTML, only its size.
type Artifact struct {
	EventID  string    `json:"event_id"`
	Title    string    `json:"title"`
	URL      string    `json:"url,omitempty"`
	HTMLSize int       `json:"html_size"`
	SavedAt  time.Time `json:"saved_at"`
	Source   string    `json:"source"`
}

// Artifacts returns an owner's saved artifacts, newest first, without
// reading their HTML.
func (s *Store) Artifacts(ctx context.Context, owner string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, json_extract(payload, '$.title'), COALESCE(json_extract(payload, '$.url'), ''),
		COALESCE(length(CAST(json_extract(payload, '$.html') AS BLOB)), 0), occurred_at, source
		FROM events WHERE owner = ? AND type = ? ORDER BY seq DESC LIMIT ?`, owner, TypeArtifactSaved, limit)
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var at string
		if err := rows.Scan(&a.EventID, &a.Title, &a.URL, &a.HTMLSize, &at, &a.Source); err != nil {
			return nil, fmt.Errorf("listing artifacts: %w", err)
		}
		if a.SavedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
