package main

import "testing"

func TestReleaseFromArtwork(t *testing.T) {
	if got := releaseFromArtwork("https://jav.example/covers/123/silo-primary?api_key=secret"); got != 123 {
		t.Fatalf("release ID = %d", got)
	}
	if got := releaseFromArtwork("https://jav.example/other/123"); got != 0 {
		t.Fatalf("unrelated artwork = %d", got)
	}
}
