// Package provider holds the JAVBeacon connection: a small HTTP client
// (client.go) plus the mutable, mutex-protected configuration the host
// delivers via Runtime.Configure. See main.go for the gRPC-facing mapping
// between this package's plain Metadata struct and the metadata_provider.v1
// / image_resolver.v1 protobuf types.
package provider

import (
	"context"
	"fmt"
	"sync"
)

// Config is the shape of the manifest's "connection" global config entry.
type Config struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// Provider wraps a *Client behind a mutex so Runtime.Configure (called
// whenever an admin saves the connection settings) can swap it out safely
// while other RPCs may be in flight. Unlike TMDB's plugin - which ships with
// a built-in API key and never needs per-installation configuration - a
// self-hosted JAVBeacon instance's URL and key are unknown until the admin
// enters them, so this plugin must actually implement Configure rather than
// treat it as a no-op.
type Provider struct {
	mu            sync.RWMutex
	client        *Client
	siloAPIKey    string
	siloBaseURL   string
	siloLibraryID string
	lastSyncedAt  string
}

// NewProvider returns an unconfigured provider. Every RPC returns a clear
// "not configured" error until Configure is called.
func NewProvider() *Provider {
	return &Provider{}
}

// Configure replaces the active JAVBeacon connection.
func (p *Provider) Configure(cfg Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = NewClient(cfg.BaseURL, cfg.APIKey, nil)
}

// ConfigureSiloAPIKey stores the API key this plugin uses to call Silo's own
// admin REST API (including v2 collection membership endpoints) - a
// separate credential from the JAVBeacon connection above, since it
// authenticates to Silo itself rather than to JAVBeacon. See the "silo_sync"
// global config entry in manifest.json.
func (p *Provider) ConfigureSiloConnection(baseURL, libraryID, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.siloAPIKey = key
	p.siloBaseURL = baseURL
	p.siloLibraryID = libraryID
}

// SiloBaseURL is the configured admin API endpoint. It avoids a callback to
// RuntimeHost.GetHostInfo from inside Silo's scheduled-task RPC.
func (p *Provider) SiloBaseURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.siloBaseURL
}

func (p *Provider) SiloLibraryID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.siloLibraryID
}

// SiloAPIKey returns the currently configured Silo API key, or "" if none has
// been set (see the "silo_sync" global config entry in manifest.json).
func (p *Provider) SiloAPIKey() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.siloAPIKey
}

// LastSyncedRevision and SetLastSyncedRevision track the last JAVBeacon
// jellyfin_library_revision value the collection-sync scheduled task acted
// on, so a run that finds the revision unchanged since the last one can skip
// straight to a no-op. This is in-memory only and resets on plugin restart,
// which simply costs one extra (harmless) full sync pass after a restart -
// Silo's own RuntimeHost has no plugin-owned durable read API to persist this
// across restarts, only SetGlobalConfigEntry's write side.
func (p *Provider) LastSyncedRevision() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastSyncedAt
}

func (p *Provider) SetLastSyncedRevision(revision string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSyncedAt = revision
}

func (p *Provider) activeClient() (*Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.client == nil || !p.client.Configured() {
		return nil, fmt.Errorf("javbeacon: connection is not configured; set base_url and api_key")
	}
	return p.client, nil
}

// Search proxies to JAVBeacon's /api/v1/integrations/silo/search.
func (p *Provider) Search(ctx context.Context, query string, limit int) ([]Metadata, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.Search(ctx, query, limit)
}

// GetMetadata proxies to JAVBeacon's /api/v1/integrations/silo/releases/{id}.
// It returns (nil, nil) when JAVBeacon has no such release.
func (p *Provider) GetMetadata(ctx context.Context, releaseID int64) (*Metadata, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.GetMetadata(ctx, releaseID)
}

// LocalReleaseCodes returns the local release ID/code index for collection sync.
func (p *Provider) LocalReleaseCodes(ctx context.Context) (map[int64]string, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.LocalReleaseCodes(ctx)
}

// GetStashMetadata resolves a Stash-only provider id.
func (p *Provider) GetStashMetadata(ctx context.Context, sceneID string) (*Metadata, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.GetStashMetadata(ctx, sceneID)
}

// LibrarySync proxies to JAVBeacon's /api/v1/integrations/silo/library-sync.
func (p *Provider) LibrarySync(ctx context.Context) (*LibrarySync, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	snapshot, err := c.LibrarySync(ctx)
	if err == nil {
		c.ClearMetadataCache()
	}
	return snapshot, err
}

// ReportPlayback proxies to JAVBeacon's /api/v1/integrations/silo/playback.
func (p *Provider) ReportPlayback(ctx context.Context, event PlaybackEvent) (*PlaybackResult, error) {
	c, err := p.activeClient()
	if err != nil {
		return nil, err
	}
	return c.ReportPlayback(ctx, event)
}

// Configured reports whether the provider currently has a usable connection,
// for the watch_sync_provider.v1 auth flow (ExchangeAPIKey/GetAccount) to
// check without triggering the "not configured" error path.
func (p *Provider) Configured() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.client != nil && p.client.Configured()
}

// ImageURL resolves a JAVBeacon-relative path into a full, authenticated URL.
// Returns "" when unconfigured rather than erroring, since ResolveImageURL
// must degrade gracefully rather than fail a whole page render over one
// image.
func (p *Provider) ImageURL(rawPath string) string {
	c, err := p.activeClient()
	if err != nil {
		return ""
	}
	return c.ImageURL(rawPath)
}

// PublicURL exposes only JAVBeacon's public routes without an API key.
func (p *Provider) PublicURL(path string) string {
	c, err := p.activeClient()
	if err != nil {
		return ""
	}
	return c.PublicURL(path)
}
