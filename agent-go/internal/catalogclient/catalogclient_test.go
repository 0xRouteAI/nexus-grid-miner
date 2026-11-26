package catalogclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchParsesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"revision": 7,
			"models": [
				{"model":{"slug":"bge-m3","capability":"embedding"},
				 "version":{"version":"1.0.0","backend":"tei-embed","min_vram_mb":4096,"recommended_vram_mb":8192}},
				{"model":{"slug":"bge-reranker-v2-m3","capability":"rerank"},
				 "version":{"version":"1.2.0","backend":"tei-rerank","min_vram_mb":6144,"recommended_vram_mb":8192}}
			]
		}`))
	}))
	defer srv.Close()

	snapshot, err := Fetch(context.Background(), srv.URL+"/api/catalog")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Revision != 7 || len(snapshot.Entries) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	entry := snapshot.Entries[1]
	if entry.Slug != "bge-reranker-v2-m3" || entry.Version != "1.2.0" ||
		entry.Capability != "rerank" || entry.MinVRAMMB != 6144 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestFetchRejectsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("HTTP 500 accepted")
	}
	if _, err := Fetch(context.Background(), ""); err == nil {
		t.Fatal("empty URL accepted")
	}
}

func TestAdvertsForFilters(t *testing.T) {
	entries := []Entry{
		{Slug: "bge-m3", Version: "1.0.0", Capability: "embedding", MinVRAMMB: 4096},
		{Slug: "big-embed", Version: "1.0.0", Capability: "embedding", MinVRAMMB: 16384},
		{Slug: "other", Version: "1.0.0", Capability: "embedding", MinVRAMMB: 1024},
	}

	got := AdvertsFor(entries, []string{"bge-m3", "big-embed"}, 8192, 512)
	if len(got) != 1 || got[0].Slug != "bge-m3" {
		t.Fatalf("adverts = %+v, want only bge-m3", got)
	}

	// Zero VRAM (detection failed) skips the gate rather than hiding models.
	got = AdvertsFor(entries, []string{"bge-m3", "big-embed"}, 0, 512)
	if len(got) != 2 {
		t.Fatalf("unknown-VRAM adverts = %+v", got)
	}
}

func TestDefaultAdverts(t *testing.T) {
	adverts := DefaultAdverts([]string{"bge-m3", "bge-reranker-v2-m3"})
	if len(adverts) != 2 {
		t.Fatalf("adverts = %+v", adverts)
	}
	if adverts[0].Key() != "bge-m3@1.0.0" || adverts[1].Key() != "bge-reranker-v2-m3@1.0.0" {
		t.Fatalf("adverts = %+v", adverts)
	}
	for _, advert := range adverts {
		if advert.Capability == "" || advert.Backend == "" {
			t.Fatalf("incomplete advert: %+v", advert)
		}
	}
}
