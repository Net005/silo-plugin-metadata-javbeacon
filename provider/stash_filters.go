package provider

import (
	"context"
	"net/url"
)

// StashSavedFilter is one StashApp SCENES saved filter and its ordered local
// scene files. It is independent of JAVBeacon's Release Library presets.
type StashSavedFilter struct {
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
	Items []StashSavedFilterItem `json:"items"`
}
type StashSavedFilterItem struct {
	SceneID string `json:"stash_scene_id"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Title   string `json:"title"`
}

func (p *Provider) StashSavedFilters(ctx context.Context, selection string) ([]StashSavedFilter, error) {
	client, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	var response struct {
		Items []StashSavedFilter `json:"items"`
	}
	if err = client.getJSON(ctx, "/api/v1/integrations/silo/stash-saved-filters?selection="+url.QueryEscape(selection), &response); err != nil {
		return nil, err
	}
	return response.Items, nil
}
