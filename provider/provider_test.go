package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderUnconfigured(t *testing.T) {
	p := NewProvider()

	if _, err := p.Search(context.Background(), "q", 10); err == nil {
		t.Fatal("expected error from Search before Configure")
	}
	if _, err := p.GetMetadata(context.Background(), 1); err == nil {
		t.Fatal("expected error from GetMetadata before Configure")
	}
	if got := p.ImageURL("/covers/1"); got != "" {
		t.Fatalf("expected empty ImageURL before Configure, got %q", got)
	}
}

func TestProviderConfigureThenSearch(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(searchResponse{Items: []Metadata{{ReleaseID: 7}}})
	})

	p := NewProvider()
	p.Configure(Config{BaseURL: srv.URL, APIKey: "secret"})

	results, err := p.Search(context.Background(), "q", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ReleaseID != 7 {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestProviderReconfigureSwapsClient(t *testing.T) {
	p := NewProvider()
	p.Configure(Config{BaseURL: "https://first.example.com", APIKey: "k1"})
	first := p.ImageURL("/covers/1")
	if want := "https://first.example.com/covers/1?api_key=k1"; first != want {
		t.Fatalf("first ImageURL = %q, want %q", first, want)
	}

	p.Configure(Config{BaseURL: "https://second.example.com", APIKey: "k2"})
	second := p.ImageURL("/covers/1")
	if want := "https://second.example.com/covers/1?api_key=k2"; second != want {
		t.Fatalf("second ImageURL = %q, want %q", second, want)
	}
}

func TestLibrarySyncCoalescesConcurrentConsumers(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/library-sync" {
			t.Errorf("path=%s", r.URL.Path)
		}
		calls++
		_, _ = w.Write([]byte(`{"revision":"r1","watched":[]}`))
	}))
	defer server.Close()
	p := NewProvider()
	p.Configure(Config{BaseURL: server.URL, APIKey: "key"})
	for i := 0; i < 2; i++ {
		if _, err := p.LibrarySync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("snapshot calls=%d, want 1", calls)
	}
	p.Configure(Config{BaseURL: server.URL, APIKey: "key"})
	if _, err := p.LibrarySync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("reconfigured snapshot calls=%d, want 2", calls)
	}
}
