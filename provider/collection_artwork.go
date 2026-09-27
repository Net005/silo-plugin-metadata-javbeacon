package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const artworkRotation = 72 * time.Hour

type artworkMarker struct {
	Bucket     int64  `json:"bucket"`
	PosterID   string `json:"poster_media_id"`
	BackdropID string `json:"backdrop_media_id"`
}

// artworkChoice selects only images belonging to this collection's current
// local members. The choice remains stable for three days so routine polls
// do not churn Silo artwork or its image cache.
func artworkChoice(spec CollectionSpec, now time.Time) (CollectionArtwork, CollectionArtwork) {
	members := make(map[string]bool, len(spec.MediaIDs))
	for _, id := range spec.MediaIDs {
		members[id] = true
	}
	posterCandidates := []CollectionArtwork{}
	backdropCandidates := []CollectionArtwork{}
	for _, art := range spec.Artwork {
		if !members[art.MediaID] {
			continue
		}
		if art.PosterURL != "" {
			posterCandidates = append(posterCandidates, art)
		}
		if art.BackdropURL != "" {
			backdropCandidates = append(backdropCandidates, art)
		}
	}
	poster := weightedArtworkPick(posterCandidates, collectionSlug(spec), "poster", now)
	if poster.BackdropURL != "" {
		return poster, poster
	}
	return poster, weightedArtworkPick(backdropCandidates, collectionSlug(spec), "backdrop", now)
}

func weightedArtworkPick(candidates []CollectionArtwork, slug, kind string, now time.Time) CollectionArtwork {
	if len(candidates) == 0 {
		return CollectionArtwork{}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := artworkDate(candidates[i]), artworkDate(candidates[j])
		if a == b {
			return candidates[i].MediaID < candidates[j].MediaID
		}
		return a > b
	})
	bucket := now.Unix() / int64(artworkRotation.Seconds())
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s:%s:%d", slug, kind, bucket)
	rng := rand.New(rand.NewSource(int64(h.Sum64())))
	pool := len(candidates)
	// Most rotations favor the newest fifth (at least three images), while
	// the rest can pick any member so older releases still appear.
	if rng.Intn(100) < 70 {
		pool = max(3, (pool+4)/5)
		if pool > len(candidates) {
			pool = len(candidates)
		}
	}
	return candidates[rng.Intn(pool)]
}

func artworkDate(art CollectionArtwork) string {
	if date := strings.TrimSpace(art.ReleaseDate); date != "" {
		return date
	}
	return strings.TrimSpace(art.AddedAt)
}

func (c *SiloClient) uploadCollectionArtwork(ctx context.Context, collectionID, kind, sourceURL string) error {
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("silo: invalid collection artwork URL")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("source_url", sourceURL); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	path := "/api/v2/admin/collections/" + url.PathEscape(collectionID) + "/" + kind
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("silo: collection %s upload returned HTTP %d", kind, resp.StatusCode)
	}
	return nil
}

func (c *SiloClient) syncCollectionArtwork(ctx context.Context, collection siloCollection, spec CollectionSpec, now time.Time) (bool, error) {
	poster, backdrop := artworkChoice(spec, now)
	if poster.PosterURL == "" && backdrop.BackdropURL == "" {
		return false, nil
	}
	bucket := now.Unix() / int64(artworkRotation.Seconds())
	path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID)
	var current siloCollection
	etag, err := c.collectionRequestETag(ctx, http.MethodGet, path, nil, &current, "")
	if err != nil {
		return false, err
	}
	var previous artworkMarker
	_ = json.Unmarshal(current.SourceConfig["javbeacon_artwork"], &previous)
	members := map[string]bool{}
	for _, id := range spec.MediaIDs {
		members[id] = true
	}
	if previous.Bucket == bucket && (previous.PosterID == "" || members[previous.PosterID]) && (previous.BackdropID == "" || members[previous.BackdropID]) {
		return false, nil
	}
	if poster.PosterURL != "" {
		if err := c.uploadCollectionArtwork(ctx, collection.ID, "poster", poster.PosterURL); err != nil {
			return false, err
		}
	}
	if backdrop.BackdropURL != "" {
		if err := c.uploadCollectionArtwork(ctx, collection.ID, "backdrop", backdrop.BackdropURL); err != nil {
			return false, err
		}
	}
	// Uploads change the collection ETag. Refresh it before saving our
	// rotation marker, preserving any other source_config values.
	etag, err = c.collectionRequestETag(ctx, http.MethodGet, path, nil, &current, "")
	if err != nil {
		return false, err
	}
	if etag == "" {
		return false, fmt.Errorf("silo: collection artwork ETag is missing")
	}
	config := current.SourceConfig
	if config == nil {
		config = map[string]json.RawMessage{}
	}
	marker, err := json.Marshal(artworkMarker{Bucket: bucket, PosterID: poster.MediaID, BackdropID: backdrop.MediaID})
	if err != nil {
		return false, err
	}
	config["javbeacon_artwork"] = marker
	if _, err := c.collectionRequestETag(ctx, http.MethodPatch, path, map[string]any{"source_config": config}, nil, etag); err != nil {
		return false, err
	}
	return true, nil
}
