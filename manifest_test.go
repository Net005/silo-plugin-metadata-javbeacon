package main

import (
	"testing"

	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// TestEmbeddedManifestIsValid guards against the embedded manifest.json
// drifting out of sync with what the SDK's own validator accepts - the same
// check the host runs when the plugin registers itself, minus the
// checksum/executable step that loadManifest performs at real startup.
func TestEmbeddedManifestIsValid(t *testing.T) {
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("embedded manifest.json failed validation: %v", err)
	}
	if m.GetPluginId() != "javbeacon.metadata" {
		t.Errorf("plugin_id = %q, want javbeacon.metadata", m.GetPluginId())
	}
	if len(m.GetCapabilities()) != 8 {
		t.Fatalf("capabilities = %d, want 8", len(m.GetCapabilities()))
	}
}

// TestLoadManifestSetsChecksum exercises the real startup path (loadManifest
// in main.go), which additionally hashes the running test binary and embeds
// version overrides.
func TestLoadManifestSetsChecksum(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.GetChecksum() == "" || m.GetChecksum() == "__CHECKSUM__" {
		t.Fatalf("expected checksum to be overwritten, got %q", m.GetChecksum())
	}
}
