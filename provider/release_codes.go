package provider

import (
	"context"
	"fmt"
)

// LocalReleaseCodes fetches release ID/code pairs in pages. Collection sync
// needs codes for every saved-filter member; querying full metadata one ID at
// a time costs thousands of HTTP requests once a preset includes local items.
func (c *Client) LocalReleaseCodes(ctx context.Context) (map[int64]string, error) {
	codes := map[int64]string{}
	const pageSize = 500
	for offset := 0; offset < 100000; offset += pageSize {
		var rows []struct {
			ID      int64  `json:"id"`
			VideoID string `json:"video_id"`
		}
		path := fmt.Sprintf("/api/releases?status=local&show_non_preferred=true&limit=%d&offset=%d", pageSize, offset)
		if err := c.getJSON(ctx, path, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.ID > 0 && row.VideoID != "" {
				codes[row.ID] = row.VideoID
			}
		}
		if len(rows) < pageSize {
			return codes, nil
		}
	}
	return nil, fmt.Errorf("javbeacon: too many local release pages")
}
