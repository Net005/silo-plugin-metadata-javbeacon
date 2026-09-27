package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// EnrichPerson uses Silo's admin person API because metadata_provider.v1's
// PersonRecord cannot carry a birth date or homepage. It only patches an
// unambiguous exact name match, and never clears a missing Stash birth date.
func (c *SiloClient) EnrichPerson(ctx context.Context, name, birthdate, homepage string) error {
	if !c.Configured() {
		return fmt.Errorf("silo: api key is not configured")
	}
	if name == "" || homepage == "" {
		return fmt.Errorf("silo: person name and homepage are required")
	}
	var profiles struct {
		Items []struct {
			ID      string `json:"id"`
			Primary bool   `json:"is_primary"`
		} `json:"items"`
	}
	if err := c.personRequest(ctx, http.MethodGet, "/api/v2/profiles", "", nil, &profiles); err != nil {
		return err
	}
	profileID := ""
	for _, p := range profiles.Items {
		if p.Primary {
			profileID = p.ID
			break
		}
	}
	if profileID == "" {
		return fmt.Errorf("silo: no primary profile")
	}
	var people struct {
		Items []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			PhotoURL string `json:"photo_url"`
		} `json:"items"`
	}
	stashID := ""
	if parts := strings.Split(strings.Trim(homepage, "/"), "/"); len(parts) >= 2 && parts[len(parts)-1] == "stash" {
		stashID = parts[len(parts)-2]
	}
	queries := []string{name}
	words := strings.Fields(name)
	if len(words) == 2 {
		queries = append(queries, words[1]+" "+words[0])
	}
	id := ""
	for _, query := range queries {
		people.Items = nil
		path := "/api/v2/catalog/people?q=" + url.QueryEscape(query) + "&limit=100"
		if err := c.personRequest(ctx, http.MethodGet, path, profileID, nil, &people); err != nil {
			return err
		}
		for _, person := range people.Items {
			if !strings.EqualFold(strings.TrimSpace(person.Name), query) {
				continue
			}
			if stashID != "" && !strings.Contains(person.PhotoURL, "/performers/"+url.PathEscape(stashID)+"/image") {
				continue
			}
			if id != "" && id != person.ID {
				return fmt.Errorf("silo: ambiguous person name %q", name)
			}
			id = person.ID
		}
	}
	if id == "" {
		return fmt.Errorf("silo: person %q not yet in catalog", name)
	}
	patch := map[string]string{"homepage": homepage}
	if birthdate != "" {
		patch["birth_date"] = birthdate
	}
	return c.personRequest(ctx, http.MethodPatch, "/api/v2/admin/people/"+url.PathEscape(id), "", patch, nil)
}

func (c *SiloClient) personRequest(ctx context.Context, method, path, profileID string, payload any, target any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if profileID != "" {
		req.Header.Set("X-Profile-Id", profileID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("silo: person request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("silo: person request HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return err
		}
	}
	return nil
}
