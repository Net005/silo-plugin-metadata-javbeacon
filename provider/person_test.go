package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnrichPersonOnlySetsKnownBirthdate(t *testing.T) {
	var patch map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/profiles", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":"primary","is_primary":true}]}`))
	})
	mux.HandleFunc("GET /api/v2/catalog/people", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Profile-Id") != "primary" {
			t.Errorf("missing primary profile")
		}
		w.Write([]byte(`{"items":[{"id":"person-1","name":"Mao Hamasaki"},{"id":"person-2","name":"Mao"}]}`))
	})
	mux.HandleFunc("PATCH /api/v2/admin/people/person-1", func(w http.ResponseWriter, r *http.Request) {
		patch = nil
		json.NewDecoder(r.Body).Decode(&patch)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewSiloClient(server.URL, "test-key")
	if err := client.EnrichPerson(context.Background(), "Mao Hamasaki", "", "https://jav.example/redirect", ""); err != nil {
		t.Fatal(err)
	}
	if patch["homepage"] != "https://jav.example/redirect" {
		t.Fatalf("homepage = %q", patch["homepage"])
	}
	if _, exists := patch["birth_date"]; exists {
		t.Fatal("unknown birth date must not clear Silo's value")
	}
	if err := client.EnrichPerson(context.Background(), "Mao Hamasaki", "1980-01-01", "https://jav.example/redirect", ""); err != nil {
		t.Fatal(err)
	}
	if patch["birth_date"] != "1980-01-01" {
		t.Fatalf("birth date = %q", patch["birth_date"])
	}
	if err := client.EnrichPerson(context.Background(), "Mao Hamasaki", "", "https://jav.example/redirect", "Country: Japan"); err != nil {
		t.Fatal(err)
	}
	if patch["bio"] != "Country: Japan" {
		t.Fatalf("bio = %q", patch["bio"])
	}
	if _, exists := patch["birth_date"]; exists {
		t.Fatal("missing birth date must not be patched")
	}
}

func TestEnrichPersonReversedNameUsesStashPortrait(t *testing.T) {
	var patched string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/profiles", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":"primary","is_primary":true}]}`))
	})
	mux.HandleFunc("GET /api/v2/catalog/people", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "Takeuchi Yuuki" {
			w.Write([]byte(`{"items":[{"id":"right","name":"Takeuchi Yuuki","photo_url":"https://jav.example/api/v1/integrations/performers/4731/image"}]}`))
		} else {
			w.Write([]byte(`{"items":[]}`))
		}
	})
	mux.HandleFunc("PATCH /api/v2/admin/people/right", func(w http.ResponseWriter, r *http.Request) { patched = r.URL.Path; w.WriteHeader(http.StatusOK) })
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewSiloClient(server.URL, "test-key")
	if err := client.EnrichPerson(context.Background(), "Yuuki Takeuchi", "1995-02-12", "https://jav.example/api/v1/integrations/performers/4731/stash", "Country: Japan"); err != nil {
		t.Fatal(err)
	}
	if patched != "/api/v2/admin/people/right" {
		t.Fatalf("patched %q", patched)
	}
}

func TestPerformerBioUsesKnownProfileFacts(t *testing.T) {
	bio := (&PerformerBio{Country: "JP", CareerLength: "2019 -", HeightCM: 158}).ProfileBio()
	if bio != "Country: Japan\nCareer: 2019 -\nHeight: 158 cm" {
		t.Fatalf("bio=%q", bio)
	}
	if got := (&PerformerBio{Details: "Existing biography", Country: "JP"}).ProfileBio(); got != "Existing biography" {
		t.Fatalf("details=%q", got)
	}
}
