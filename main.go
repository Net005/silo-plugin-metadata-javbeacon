package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

const capabilityID = "javbeacon"

// stashSceneIDProviderKeyLower is the lowercase provider_ids key this plugin
// publishes for a release's linked StashApp scene (see providerIDsStruct
// below) - the counterpart watchsync.go reads back out of a WatchSyncMedia's
// external_ids to support the Stash-only playback path (no JAVBeacon release
// row at all) even when releaseIDFromExternalIDs finds nothing.
const stashSceneIDProviderKeyLower = "stash"

// runtimeServer embeds runtimedefault.Server for its BindHostBroker handler
// and additionally implements Configure, since (unlike TMDB's plugin, which
// ships a built-in API key) a self-hosted JAVBeacon instance's URL and key
// are unknown until an admin enters them.
type runtimeServer struct {
	runtimedefault.Server
	manifest       *pluginv1.PluginManifest
	provider       *provider.Provider
	collectionSync *collectionSyncTaskServer
	pollOnce       sync.Once
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure decodes the "connection" global config entry (base_url, api_key)
// and swaps it into the provider. Entries the host sends for other keys (none
// declared here) are ignored rather than rejected, matching how additive
// config keys are meant to be handled.
func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	for _, entry := range req.GetConfig() {
		values := entry.GetValue().AsMap()
		switch entry.GetKey() {
		case "connection":
			s.provider.Configure(provider.Config{
				BaseURL: stringValue(values["base_url"]),
				APIKey:  stringValue(values["api_key"]),
			})
		case "silo_sync":
			s.provider.ConfigureSiloConnection(stringValue(values["silo_base_url"]), stringValue(values["silo_library_id"]), stringValue(values["silo_api_key"]))
		}
	}
	if s.collectionSync != nil {
		s.pollOnce.Do(func() { go s.collectionSync.poll() })
	}
	return &pluginv1.ConfigureResponse{}, nil
}

func stringValue(raw any) string {
	s, _ := raw.(string)
	return s
}

type metadataServer struct {
	personOnce sync.Once
	personJobs chan personJob
	personSeen sync.Map
	log        hclog.Logger
	pluginv1.UnimplementedMetadataProviderServer
	pluginv1.UnimplementedImageResolverServer
	runtime *runtimeServer
}

// supportsItemType reports whether this provider serves the given item_type.
// JAVBeacon tracks single-file releases only - no series/season/episode
// structure - so it is presented to Silo as a movie provider. An empty
// item_type (some callers omit it) is treated as "movie" too.
func supportsItemType(itemType string) bool {
	return itemType == "" || itemType == "movie"
}

func (s *metadataServer) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	if !supportsItemType(req.GetItemType()) {
		return &pluginv1.SearchMetadataResponse{}, nil
	}

	if sceneID, ok := stashProviderID("", req.GetProviderIds()); ok {
		item, err := s.runtime.provider.GetStashMetadata(ctx, sceneID)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return &pluginv1.SearchMetadataResponse{}, nil
		}
		return &pluginv1.SearchMetadataResponse{Results: []*pluginv1.ProviderSearchResult{searchResultFromMetadata(item)}}, nil
	}

	// A direct JAVBeacon release id (via provider_ids) skips the search
	// endpoint entirely and fetches the one release, mirroring how TMDB's
	// plugin prefers a direct id lookup over a title search.
	if releaseID := releaseIDFromProto(req.GetProviderIds()); releaseID != 0 {
		item, err := s.runtime.provider.GetMetadata(ctx, releaseID)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return &pluginv1.SearchMetadataResponse{}, nil
		}
		return &pluginv1.SearchMetadataResponse{Results: []*pluginv1.ProviderSearchResult{searchResultFromMetadata(item)}}, nil
	}

	results, err := s.runtime.provider.Search(ctx, req.GetQuery(), 25)
	if err != nil {
		return nil, err
	}
	response := &pluginv1.SearchMetadataResponse{Results: make([]*pluginv1.ProviderSearchResult, 0, len(results))}
	for i := range results {
		response.Results = append(response.Results, searchResultFromMetadata(&results[i]))
	}
	return response, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	if !supportsItemType(req.GetItemType()) {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	releaseID := releaseIDFromProto(req.GetProviderIds())
	if releaseID == 0 {
		releaseID = parseInt64(req.GetProviderId())
	}
	var item *provider.Metadata
	var err error
	if sceneID, ok := stashProviderID(req.GetProviderId(), req.GetProviderIds()); ok {
		item, err = s.runtime.provider.GetStashMetadata(ctx, sceneID)
	} else if releaseID != 0 {
		item, err = s.runtime.provider.GetMetadata(ctx, releaseID)
	}
	if err != nil || item == nil {
		return &pluginv1.GetMetadataResponse{}, err
	}
	s.queuePeople(item)
	if item.ProviderID != "" && req.GetFilePath() != "" {
		copy := *item
		base := filepath.Base(req.GetFilePath())
		copy.Code = strings.TrimSuffix(base, filepath.Ext(base))
		item = &copy
	}
	return &pluginv1.GetMetadataResponse{Item: metadataItemFromResult(item)}, nil
}

type personJob struct {
	name   string
	detail provider.PerformerDetail
}

// Silo can write the same person again during a full library refresh. Keep
// immediate updates inexpensive, but always schedule one final patch when
// another metadata request arrives during the per-person cooldown.
type personRefreshState struct {
	mu      sync.Mutex
	next    time.Time
	pending bool
	latest  personJob
}

const personRefreshInterval = time.Minute

func (s *metadataServer) queuePeople(item *provider.Metadata) {
	if len(item.PerformerDetails) == 0 || s.runtime.provider.SiloAPIKey() == "" {
		return
	}
	s.personOnce.Do(func() {
		s.personJobs = make(chan personJob, 1024)
		for range 4 {
			go s.personWorker()
		}
	})
	for _, name := range item.Performers {
		detail, ok := item.PerformerDetails[name]
		if !ok || detail.StashID == "" {
			continue
		}
		s.enqueuePerson(personJob{name: name, detail: detail})
	}
}

func (s *metadataServer) enqueuePerson(job personJob) {
	value, _ := s.personSeen.LoadOrStore(job.detail.StashID, &personRefreshState{})
	state := value.(*personRefreshState)
	state.mu.Lock()
	state.latest = job
	if state.pending {
		state.mu.Unlock()
		return
	}
	if delay := time.Until(state.next); delay > 0 {
		state.pending = true
		state.mu.Unlock()
		time.AfterFunc(delay, func() { s.flushPerson(state) })
		return
	}
	state.next = time.Now().Add(personRefreshInterval)
	state.mu.Unlock()
	s.sendPersonJob(job)
}

func (s *metadataServer) flushPerson(state *personRefreshState) {
	state.mu.Lock()
	job := state.latest
	state.pending = false
	state.next = time.Now().Add(personRefreshInterval)
	state.mu.Unlock()
	s.sendPersonJob(job)
}

func (s *metadataServer) sendPersonJob(job personJob) {
	select {
	case s.personJobs <- job:
	default:
		s.personSeen.Delete(job.detail.StashID)
	}
}

func (s *metadataServer) personWorker() {
	for job := range s.personJobs {
		// Silo writes the cast after GetMetadata returns. Retry while that
		// write is settling, then allow a future fetch to enqueue again.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		var err error
		for attempt, delay := range []time.Duration{3 * time.Second, 5 * time.Second, 15 * time.Second} {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				err = ctx.Err()
				break
			}
			host := sdkruntime.Host()
			if host == nil {
				err = fmt.Errorf("runtime host is not bound")
				break
			}
			info, hostErr := host.GetHostInfo(ctx)
			if hostErr != nil {
				err = hostErr
				break
			}
			homepage := s.runtime.provider.PublicURL("/api/v1/integrations/performers/" + url.PathEscape(job.detail.StashID) + "/stash")
			err = provider.NewSiloClient(info.InternalBaseURL, s.runtime.provider.SiloAPIKey()).EnrichPerson(ctx, job.name, job.detail.Birthdate, homepage)
			if err == nil || attempt == 2 {
				break
			}
		}
		if err != nil {
			s.personSeen.Delete(job.detail.StashID)
			if s.log != nil {
				s.log.Warn("performer enrichment failed", "name", job.name, "err", err)
			}
		}
		cancel()
	}
}

// PersonRecord has no custom provider_ids field. Store the namespaced Stash
// identity in plex_guid so Silo can pass it back in GetPersonDetail. The
// namespaced value is never a real Plex GUID.
func stashPersonID(ids *structpb.Struct) string {
	if ids == nil {
		return ""
	}
	values := ids.AsMap()
	for _, key := range []string{stashSceneIDProviderKeyLower, capabilityID, "plex", "plex_guid"} {
		value, _ := values[key].(string)
		value = strings.TrimSpace(value)
		if id, ok := strings.CutPrefix(value, "stash:"); ok && id != "" {
			return id
		}
		if key == stashSceneIDProviderKeyLower && value != "" {
			return value
		}
	}
	// Silo passes the original portrait URL when an older person has no
	// persisted Stash identity yet. Use only the stable performer image route.
	photoSource, _ := values["photo_source"].(string)
	parsed, err := url.Parse(photoSource)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 6 && strings.Join(parts[:4], "/") == "api/v1/integrations/performers" && parts[5] == "image" {
			if id, err := strconv.ParseInt(parts[4], 10, 64); err == nil && id > 0 {
				return strconv.FormatInt(id, 10)
			}
		}
	}
	return ""
}

func (s *metadataServer) GetPersonDetail(ctx context.Context, req *pluginv1.GetPersonDetailRequest) (*pluginv1.GetPersonDetailResponse, error) {
	id := stashPersonID(req.GetProviderIds())
	if id == "" {
		return &pluginv1.GetPersonDetailResponse{}, nil
	}
	bio, err := s.runtime.provider.GetPerformerBio(ctx, id)
	if err != nil {
		return nil, err
	}
	ids, _ := structpb.NewStruct(map[string]any{stashSceneIDProviderKeyLower: id, capabilityID: "stash:" + id, "plex": "stash:" + id})
	return &pluginv1.GetPersonDetailResponse{Person: &pluginv1.PersonDetailRecord{
		Name:        bio.Name,
		Bio:         bio.Details,
		BirthDate:   bio.Birthdate,
		DeathDate:   bio.DeathDate,
		Homepage:    s.runtime.provider.PublicURL("/api/v1/integrations/performers/" + url.PathEscape(id) + "/stash"),
		PhotoPath:   javbeaconCanonicalPath("/api/v1/integrations/performers/" + url.PathEscape(id) + "/image"),
		ProviderIds: ids,
	}}, nil
}

func (s *metadataServer) GetSeasons(context.Context, *pluginv1.GetSeasonsRequest) (*pluginv1.GetSeasonsResponse, error) {
	return &pluginv1.GetSeasonsResponse{}, nil
}

func (s *metadataServer) GetEpisodes(context.Context, *pluginv1.GetEpisodesRequest) (*pluginv1.GetEpisodesResponse, error) {
	return &pluginv1.GetEpisodesResponse{}, nil
}

func (s *metadataServer) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	if !supportsItemType(req.GetItemType()) {
		return &pluginv1.GetImagesResponse{}, nil
	}
	releaseID := releaseIDFromProto(req.GetProviderIds())
	if releaseID == 0 {
		releaseID = parseInt64(req.GetProviderId())
	}
	var item *provider.Metadata
	var err error
	if sceneID, ok := stashProviderID(req.GetProviderId(), req.GetProviderIds()); ok {
		item, err = s.runtime.provider.GetStashMetadata(ctx, sceneID)
	} else if releaseID != 0 {
		item, err = s.runtime.provider.GetMetadata(ctx, releaseID)
	}
	if err != nil || item == nil {
		return &pluginv1.GetImagesResponse{}, err
	}

	return &pluginv1.GetImagesResponse{Images: imagesFromMetadata(item)}, nil
}

// imagesFromMetadata builds GetImages' candidate list from item. Split out
// from GetImages so the image-selection rules (which fields become which
// image "kind", and how StashScreenshotURL rides alongside rather than
// replaces the others) are unit-testable without a provider round trip.
func imagesFromMetadata(item *provider.Metadata) []*pluginv1.ImageRecord {
	var images []*pluginv1.ImageRecord
	if item.CoverPath != "" {
		images = append(images, &pluginv1.ImageRecord{
			Kind: "poster",
			Url:  javbeaconCanonicalPath(item.CoverPath),
		})
	}
	if item.CoverBackdropPath != "" {
		images = append(images, &pluginv1.ImageRecord{
			Kind: "backdrop",
			Url:  javbeaconCanonicalPath(item.CoverBackdropPath),
		})
	}
	// Screenshots ride along as additional backdrop-kind images - JAVBeacon
	// has no separate "gallery" image kind, and a scene's screenshots make a
	// reasonable backdrop rotation, the same role TMDB's extra backdrops
	// play for movies.
	for _, path := range item.BackdropURLs {
		images = append(images, &pluginv1.ImageRecord{
			Kind: "backdrop",
			Url:  javbeaconCanonicalPath(path),
		})
	}
	// StashScreenshotURL is only populated when JAVBeacon has no scraped
	// cover of its own for this release (see provider/types.go), so it rides
	// along as an ADDITIONAL poster/backdrop candidate alongside - never
	// instead of - CoverPath/CoverBackdropPath above, the same fallback role
	// it already plays for the Jellyfin plugin. New in JAVBeacon v1.0.239;
	// this plugin previously had no Stash-screenshot gap-fill at all.
	if item.StashScreenshotURL != "" {
		poster := firstNonempty(item.StashPosterURL, item.StashScreenshotURL)
		if poster != item.CoverPath {
			images = append(images, &pluginv1.ImageRecord{Kind: "poster", Url: javbeaconCanonicalPath(poster)})
		}
		if item.StashScreenshotURL != item.CoverBackdropPath {
			images = append(images, &pluginv1.ImageRecord{Kind: "backdrop", Url: javbeaconCanonicalPath(item.StashScreenshotURL)})
		}
	}
	return images
}

// ResolveImageURL and ResolveImageURLs ignore the variant size hint on
// purpose: JAVBeacon does not generate multiple resolutions of a cover or
// screenshot the way TMDB's CDN does, so there is nothing to pick between.
// See Client.ImageURL's doc comment for why that is spec-compliant rather
// than a shortcut.
func (s *metadataServer) ResolveImageURL(_ context.Context, req *pluginv1.ResolveImageURLRequest) (*pluginv1.ResolveImageURLResponse, error) {
	rawPath := strings.TrimPrefix(req.GetPath(), "javbeacon://")
	return &pluginv1.ResolveImageURLResponse{Url: s.runtime.provider.ImageURL("/" + rawPath)}, nil
}

func (s *metadataServer) ResolveImageURLs(_ context.Context, req *pluginv1.ResolveImageURLsRequest) (*pluginv1.ResolveImageURLsResponse, error) {
	urls := make(map[string]string, len(req.GetPaths()))
	for _, path := range req.GetPaths() {
		rawPath := strings.TrimPrefix(path, "javbeacon://")
		urls[path] = s.runtime.provider.ImageURL("/" + rawPath)
	}
	return &pluginv1.ResolveImageURLsResponse{Urls: urls}, nil
}

// javbeaconCanonicalPath wraps a JAVBeacon-relative path (as returned in
// Metadata.CoverPath etc.) with the javbeacon:// scheme so the host can
// resolve it later via ResolveImageURL. Returns "" for an empty path.
func javbeaconCanonicalPath(rawPath string) string {
	if rawPath == "" {
		return ""
	}
	return "javbeacon://" + strings.TrimPrefix(rawPath, "/")
}

func searchResultFromMetadata(item *provider.Metadata) *pluginv1.ProviderSearchResult {
	return &pluginv1.ProviderSearchResult{
		ProviderId:  metadataProviderID(item),
		ItemType:    "movie",
		Title:       item.Code,
		Overview:    item.Overview,
		ProviderIds: providerIDsStruct(item),
		ImageUrl:    javbeaconCanonicalPath(item.CoverPath),
		Year:        int32(item.ProductionYear),
	}
}

func metadataItemFromResult(item *provider.Metadata) *pluginv1.MetadataItem {
	genres := make([]string, 0, len(item.Genres))
	for _, genre := range item.Genres {
		label := strings.TrimSpace(genre)
		if strings.EqualFold(label, "Watchlist") || strings.HasPrefix(strings.ToLower(label), "collection: ") {
			continue
		}
		genres = append(genres, genre)
	}
	out := &pluginv1.MetadataItem{
		ProviderId:    metadataProviderID(item),
		ItemType:      "movie",
		Title:         item.Code,
		OriginalTitle: item.OriginalTitle,
		SortTitle:     item.Code,
		Year:          int32(item.ProductionYear),
		Overview:      item.Overview,
		Runtime:       int32(item.RuntimeSeconds / 60),
		Genres:        genres,
		ProviderIds:   providerIDsStruct(item),
		ReleaseDate:   item.PremiereDate,
		PosterPath:    javbeaconCanonicalPath(item.CoverPath),
		BackdropPath:  javbeaconCanonicalPath(item.CoverBackdropPath),
		People:        peopleFromResult(item, item.PerformerImages),
	}
	if item.Studio != "" {
		out.Studios = []string{item.Studio}
	}
	return out
}

// peopleFromResult builds Silo's People list, attaching a photo (via the same
// javbeacon:// canonical-path + ResolveImageURL scheme used for posters and
// backdrops) to any performer StashApp has a portrait for. JAVBeacon itself
// never scrapes performer photos, so performerImages (from
// Metadata.PerformerImages, keyed by performer name) is the only source.
func peopleFromResult(item *provider.Metadata, performerImages map[string]string) []*pluginv1.PersonRecord {
	var people []*pluginv1.PersonRecord
	for i, name := range item.Performers {
		person := &pluginv1.PersonRecord{
			Name:      name,
			Kind:      "actor",
			SortOrder: int32(i),
		}
		if detail, ok := item.PerformerDetails[name]; ok && detail.StashID != "" {
			person.PlexGuid = "stash:" + detail.StashID
		}
		if path, ok := performerImages[name]; ok && path != "" {
			person.PhotoPath = javbeaconCanonicalPath(path)
		}
		people = append(people, person)
	}
	for _, name := range item.Directors {
		people = append(people, &pluginv1.PersonRecord{
			Name: name,
			Kind: "director",
		})
	}
	return people
}

func providerIDsStruct(item *provider.Metadata) *structpb.Struct {
	ids := map[string]any{capabilityID: metadataProviderID(item)}
	if item.StashSceneID != "" {
		ids[stashSceneIDProviderKeyLower] = item.StashSceneID
	}
	out, err := structpb.NewStruct(ids)
	if err != nil {
		return nil
	}
	return out
}

func metadataProviderID(item *provider.Metadata) string {
	if item.ProviderID != "" {
		return item.ProviderID
	}
	return strconv.FormatInt(item.ReleaseID, 10)
}

func stashProviderID(raw string, ids *structpb.Struct) (string, bool) {
	if ids != nil {
		values := ids.AsMap()
		if value, ok := values[stashSceneIDProviderKeyLower].(string); ok && value != "" {
			raw = "stash:" + value
		} else if value, ok := values[capabilityID].(string); ok && strings.HasPrefix(value, "stash:") {
			raw = value
		}
	}
	sceneID, ok := strings.CutPrefix(raw, "stash:")
	return sceneID, ok && sceneID != ""
}

// releaseIDFromProto reads this plugin's own provider id (keyed by
// capabilityID, "javbeacon") out of a request's provider_ids struct.
func releaseIDFromProto(value *structpb.Struct) int64 {
	if value == nil {
		return 0
	}
	raw, ok := value.AsMap()[capabilityID]
	if !ok {
		return 0
	}
	text, ok := raw.(string)
	if !ok {
		return 0
	}
	return parseInt64(text)
}

func parseInt64(text string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}

	// logger was never wired into ServeConfig before this - hclog defaulted
	// to an internal instance the plugin itself never wrote to, so nothing
	// this plugin did was ever visible in Silo's own Logs page regardless of
	// what actually went wrong. See collectionsync.go/matchsync.go for where
	// this now gets used (both scheduled tasks log each network call's
	// outcome and duration).
	logger := hclog.New(&hclog.LoggerOptions{Name: "javbeacon-metadata", Level: hclog.Info})

	rs := &runtimeServer{manifest: manifest, provider: provider.NewProvider()}
	ms := &metadataServer{runtime: rs, log: logger}
	ws := &watchSyncServer{runtime: rs}
	cs := &collectionSyncTaskServer{runtime: rs, log: logger}
	rs.collectionSync = cs

	runtime.Serve(runtime.ServeConfig{
		Logger: logger,
		Servers: runtime.CapabilityServers{
			Runtime:           rs,
			MetadataProvider:  ms,
			ImageResolver:     ms,
			WatchSyncProvider: ws,
			ScheduledTask:     cs,
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}

	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])

	return manifest, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
