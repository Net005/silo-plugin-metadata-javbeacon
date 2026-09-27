package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
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
	manifest *pluginv1.PluginManifest
	provider *provider.Provider
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
			s.provider.ConfigureSiloAPIKey(stringValue(values["silo_api_key"]))
		}
	}
	return &pluginv1.ConfigureResponse{}, nil
}

func stringValue(raw any) string {
	s, _ := raw.(string)
	return s
}

type metadataServer struct {
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
	if releaseID == 0 {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	item, err := s.runtime.provider.GetMetadata(ctx, releaseID)
	if err != nil || item == nil {
		return &pluginv1.GetMetadataResponse{}, err
	}
	return &pluginv1.GetMetadataResponse{Item: metadataItemFromResult(item)}, nil
}

// GetPersonDetail, GetSeasons, and GetEpisodes have nothing to return:
// JAVBeacon does not track performer biographies/external ids, and releases
// have no season/episode structure. Returning an empty response (rather than
// an error) lets the host move on without treating this as a failed call.
func (s *metadataServer) GetPersonDetail(context.Context, *pluginv1.GetPersonDetailRequest) (*pluginv1.GetPersonDetailResponse, error) {
	return &pluginv1.GetPersonDetailResponse{}, nil
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
	if releaseID == 0 {
		return &pluginv1.GetImagesResponse{}, nil
	}
	item, err := s.runtime.provider.GetMetadata(ctx, releaseID)
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
		images = append(images,
			&pluginv1.ImageRecord{Kind: "poster", Url: javbeaconCanonicalPath(item.StashScreenshotURL)},
			&pluginv1.ImageRecord{Kind: "backdrop", Url: javbeaconCanonicalPath(item.StashScreenshotURL)},
		)
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
		ProviderId:  strconv.FormatInt(item.ReleaseID, 10),
		ItemType:    "movie",
		Title:       item.Code,
		Overview:    item.Overview,
		ProviderIds: providerIDsStruct(item),
		ImageUrl:    javbeaconCanonicalPath(item.CoverPath),
		Year:        int32(item.ProductionYear),
	}
}

func metadataItemFromResult(item *provider.Metadata) *pluginv1.MetadataItem {
	genres := append([]string(nil), item.Genres...)
	// JAVBeacon saved filter sets ("collections" in Jellyfin) have no
	// equivalent Silo plugin capability - metadata_provider.v1 carries no
	// collection concept at all - so each one the release belongs to rides
	// along as an extra, clearly-prefixed genre entry instead, which the user
	// can filter/browse on in Silo like any other genre.
	for _, name := range item.CollectionNames {
		if name == "" {
			continue
		}
		genres = append(genres, "Collection: "+name)
	}
	// Same substitute as above, for the one JAVBeacon "collection" that isn't
	// a saved filter set: Silo has no favorites/watchlist marker of its own
	// for this plugin to set, so Watchlist membership rides along as a plain
	// genre entry too, filterable/browsable the same way.
	if item.Watchlist {
		genres = append(genres, "Watchlist")
	}
	out := &pluginv1.MetadataItem{
		ProviderId:    strconv.FormatInt(item.ReleaseID, 10),
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
	ids := map[string]any{capabilityID: strconv.FormatInt(item.ReleaseID, 10)}
	if item.StashSceneID != "" {
		ids[stashSceneIDProviderKeyLower] = item.StashSceneID
	}
	out, err := structpb.NewStruct(ids)
	if err != nil {
		return nil
	}
	return out
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
	ms := &metadataServer{runtime: rs}
	ws := &watchSyncServer{runtime: rs}
	cs := &collectionSyncTaskServer{runtime: rs, log: logger}

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
