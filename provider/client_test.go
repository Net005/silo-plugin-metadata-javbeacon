package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientSearch(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/search" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "abc-123" {
			t.Fatalf("unexpected q=%q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Fatalf("unexpected limit=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("unexpected Authorization=%q", got)
		}
		_ = json.NewEncoder(w).Encode(searchResponse{
			Items: []Metadata{{ReleaseID: 1, Code: "ABC-123"}},
			Total: 1,
		})
	})

	c := NewClient(srv.URL, "secret", nil)
	results, err := c.Search(context.Background(), "abc-123", 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Code != "ABC-123" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestClientGetMetadataFound(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/releases/42" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(Metadata{ReleaseID: 42, Code: "XYZ-999"})
	})

	c := NewClient(srv.URL, "secret", nil)
	item, err := c.GetMetadata(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if item == nil || item.Code != "XYZ-999" {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestClientGetMetadataNotFound(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	c := NewClient(srv.URL, "secret", nil)
	item, err := c.GetMetadata(context.Background(), 42)
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if item != nil {
		t.Fatalf("expected nil item on 404, got %+v", item)
	}
}

func TestClientGetMetadataServerError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	c := NewClient(srv.URL, "secret", nil)
	_, err := c.GetMetadata(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestClientImageURL(t *testing.T) {
	c := NewClient("https://jav.example.com/", "secret", nil)
	got := c.ImageURL("/covers/1/jellyfin-primary")
	want := "https://jav.example.com/covers/1/jellyfin-primary?api_key=secret"
	if got != want {
		t.Fatalf("ImageURL = %q, want %q", got, want)
	}

	// Missing leading slash gets one added.
	got = c.ImageURL("covers/1/jellyfin-primary")
	if got != want {
		t.Fatalf("ImageURL (no leading slash) = %q, want %q", got, want)
	}
}

func TestClientImageURLUnconfiguredBaseURL(t *testing.T) {
	c := NewClient("", "secret", nil)
	if got := c.ImageURL("/covers/1"); got != "" {
		t.Fatalf("expected empty URL with no base_url, got %q", got)
	}
}

func TestClientConfigured(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		apiKey  string
		want    bool
	}{
		{"both set", "https://jav.example.com", "key", true},
		{"missing base url", "", "key", false},
		{"missing api key", "https://jav.example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(tc.baseURL, tc.apiKey, nil)
			if got := c.Configured(); got != tc.want {
				t.Fatalf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}
