package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CompletedPlayback is one finalized movie attempt in Silo's admin history.
type CompletedPlayback struct {
	SessionID   string    `json:"session_id"`
	MediaItemID string    `json:"media_item_id"`
	MediaType   string    `json:"media_type"`
	EndedAt     time.Time `json:"ended_at"`
	Completed   bool      `json:"completed"`
}

// CompletedPlaybackHistory consumes the complete cursor sequence before any
// caller writes to Stash. A partial page set must never become a backfill.
func (c *SiloClient) CompletedPlaybackHistory(ctx context.Context, profileID string) ([]CompletedPlayback, error) {
	if !c.Configured() || profileID == "" {
		return nil, fmt.Errorf("silo: API key and profile are required")
	}
	result := []CompletedPlayback{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		values := url.Values{"limit": {"200"}, "completed": {"true"}, "profile_id": {profileID}}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/admin/playback-history?"+values.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("silo: playback history HTTP %d", resp.StatusCode)
		}
		var body struct {
			Items []CompletedPlayback `json:"items"`
			Page  struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		result = append(result, body.Items...)
		if !body.Page.HasMore {
			return result, nil
		}
		if body.Page.NextCursor == "" || seen[body.Page.NextCursor] {
			return nil, fmt.Errorf("silo: invalid playback history cursor")
		}
		seen[body.Page.NextCursor] = true
		cursor = body.Page.NextCursor
	}
	return nil, fmt.Errorf("silo: playback history exceeded 100 pages")
}
