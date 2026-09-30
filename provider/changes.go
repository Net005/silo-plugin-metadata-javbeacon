package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MetadataChange is a local JAVBeacon release or Stash scene changed since a
// cursor. The Silo plugin resolves these identities against its local catalog.
type MetadataChange struct {
	ReleaseID    int64  `json:"release_id"`
	StashSceneID string `json:"stash_scene_id"`
	Code         string `json:"code"`
	Title        string `json:"title"`
	Path         string `json:"path"`
}

type MetadataChanges struct {
	CheckedAt time.Time        `json:"checked_at"`
	Items     []MetadataChange `json:"items"`
}

func (c *Client) MetadataChanges(ctx context.Context, since time.Time) (*MetadataChanges, error) {
	var result MetadataChanges
	path := "/api/v1/integrations/silo/metadata-changes"
	if !since.IsZero() {
		path += "?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339Nano))
	}
	err := c.getJSON(ctx, path, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (p *Provider) MetadataChanges(ctx context.Context, since time.Time) (*MetadataChanges, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.MetadataChanges(ctx, since)
}

func (p *Provider) ClearMetadataCache() {
	p.mu.RLock()
	c := p.client
	p.mu.RUnlock()
	if c != nil {
		c.ClearMetadataCache()
	}
}

func (c *Client) AckMetadataChanges(ctx context.Context, checkedAt time.Time) error {
	raw, err := json.Marshal(map[string]time.Time{"checked_at": checkedAt})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/integrations/silo/metadata-changes/ack", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("javbeacon: metadata change acknowledgement HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (p *Provider) AckMetadataChanges(ctx context.Context, checkedAt time.Time) error {
	c, err := p.activeClient()
	if err != nil {
		return err
	}
	return c.AckMetadataChanges(ctx, checkedAt)
}
