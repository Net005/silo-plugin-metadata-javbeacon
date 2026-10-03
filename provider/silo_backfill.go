package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type SiloBackfillPlay struct {
	SessionID string    `json:"session_id"`
	EndedAt   time.Time `json:"ended_at"`
}

type SiloBackfillResult struct {
	Added          int    `json:"added"`
	AlreadyPresent int    `json:"already_present"`
	Skipped        int    `json:"skipped"`
	Reason         string `json:"reason"`
}

func (p *Provider) BackfillSiloPlays(ctx context.Context, sceneID string, plays []SiloBackfillPlay) (*SiloBackfillResult, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		SceneID string             `json:"scene_id"`
		Plays   []SiloBackfillPlay `json:"plays"`
	}{sceneID, plays})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/integrations/silo/playback/backfill", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("javbeacon: backfill HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var result SiloBackfillResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
