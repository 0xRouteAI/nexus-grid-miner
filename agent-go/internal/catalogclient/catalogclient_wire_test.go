package catalogclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// catalog.json is the shape the control plane's public /api/catalog returns,
// shared byte-identically with the server repository.
func TestFetchReadsWireFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "wire", "catalog.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	snapshot, err := Fetch(context.Background(), srv.URL+"/api/catalog")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Revision != 7 {
		t.Fatalf("revision = %d", snapshot.Revision)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %+v", snapshot.Entries)
	}

	entry := snapshot.Entries[0]
	if entry.Slug != "bge-m3" || entry.Version != "1.0.0" ||
		entry.Capability != "embedding" {
		t.Fatalf("entry = %+v", entry)
	}
	// model_ref is used for the local identity check, so the key must survive.
	if entry.ModelRef != "BAAI/bge-m3" {
		t.Fatalf("model_ref = %q", entry.ModelRef)
	}
	if entry.Backend != "tei-embed" {
		t.Fatalf("backend = %q", entry.Backend)
	}
	if entry.MinVRAMMB != 2048 || entry.RecommendedVRAMMB != 4096 {
		t.Fatalf("vram fields = %d/%d", entry.MinVRAMMB, entry.RecommendedVRAMMB)
	}

	// The advert the agent would register with keeps model_ref too.
	adverts := AdvertsFor(snapshot.Entries, []string{"bge-m3"}, 0, 0)
	if len(adverts) != 1 || adverts[0].Key() != "bge-m3@1.0.0" {
		t.Fatalf("adverts = %+v", adverts)
	}
	if !bytes.Contains([]byte(adverts[0].ModelRef), []byte("BAAI/bge-m3")) {
		t.Fatalf("advert model_ref = %q", adverts[0].ModelRef)
	}
}
