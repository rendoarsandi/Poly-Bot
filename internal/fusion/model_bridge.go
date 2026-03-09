package fusion

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type externalModelScore struct {
	Score      float64
	Confidence float64
	Reason     string
	UpdatedAt  time.Time
}

type externalScoreProvider struct {
	path      string
	mu        sync.RWMutex
	lastCheck time.Time
	lastMod   time.Time
	scores    map[string]externalModelScore
	lastErr   string
}

type externalScoreFile struct {
	UpdatedAt string                            `json:"updated_at"`
	Scores    map[string]externalScoreFileEntry `json:"scores"`
}

type externalScoreFileEntry struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	UpdatedAt  string  `json:"updated_at"`
}

func newExternalScoreProvider(path string) *externalScoreProvider {
	return &externalScoreProvider{path: path, scores: map[string]externalModelScore{}}
}

func (p *externalScoreProvider) Lookup(asset string) (externalModelScore, bool) {
	if p == nil {
		return externalModelScore{}, false
	}
	p.refreshIfNeeded()
	p.mu.RLock()
	defer p.mu.RUnlock()
	score, ok := p.scores[strings.ToUpper(strings.TrimSpace(asset))]
	return score, ok
}

func (p *externalScoreProvider) LastError() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastErr
}

func (p *externalScoreProvider) refreshIfNeeded() {
	p.mu.Lock()
	if time.Since(p.lastCheck) < time.Second {
		p.mu.Unlock()
		return
	}
	p.lastCheck = time.Now()
	p.mu.Unlock()

	info, err := os.Stat(p.path)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err.Error()
		p.mu.Unlock()
		return
	}

	p.mu.RLock()
	unchanged := info.ModTime().Equal(p.lastMod)
	p.mu.RUnlock()
	if unchanged {
		return
	}

	data, err := os.ReadFile(p.path)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err.Error()
		p.mu.Unlock()
		return
	}

	scores, err := parseExternalScores(data)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastErr = err.Error()
		return
	}
	p.scores = scores
	p.lastMod = info.ModTime()
	p.lastErr = ""
}

func parseExternalScores(data []byte) (map[string]externalModelScore, error) {
	var wrapped externalScoreFile
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Scores) > 0 {
		return normalizeExternalScores(wrapped.Scores, wrapped.UpdatedAt)
	}
	var direct map[string]externalScoreFileEntry
	if err := json.Unmarshal(data, &direct); err != nil {
		return nil, fmt.Errorf("parse external scores: %w", err)
	}
	return normalizeExternalScores(direct, "")
}

func normalizeExternalScores(entries map[string]externalScoreFileEntry, fallbackUpdatedAt string) (map[string]externalModelScore, error) {
	out := make(map[string]externalModelScore, len(entries))
	for asset, entry := range entries {
		key := strings.ToUpper(strings.TrimSpace(asset))
		if key == "" {
			continue
		}
		updatedAt := time.Time{}
		rawUpdatedAt := strings.TrimSpace(entry.UpdatedAt)
		if rawUpdatedAt == "" {
			rawUpdatedAt = strings.TrimSpace(fallbackUpdatedAt)
		}
		if rawUpdatedAt != "" {
			parsed, err := time.Parse(time.RFC3339, rawUpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("parse updated_at for %s: %w", key, err)
			}
			updatedAt = parsed
		}
		out[key] = externalModelScore{
			Score:      clamp(entry.Score, -0.30, 0.30),
			Confidence: clamp(entry.Confidence, 0, 1),
			Reason:     strings.TrimSpace(entry.Reason),
			UpdatedAt:  updatedAt,
		}
	}
	return out, nil
}
