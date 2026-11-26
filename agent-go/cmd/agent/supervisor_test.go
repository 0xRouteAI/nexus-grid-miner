package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/gpu"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/teitest"
)

// TestSupervisorPicksUpCatalogVersionChange: the operator bumps bge-m3 from
// 1.0.0 to 1.0.1 (and retires 1.0.0). A running agent must notice on its next
// catalog refresh and ask to re-register, without a process restart.
func TestSupervisorPicksUpCatalogVersionChange(t *testing.T) {
	var version atomic.Value
	version.Store("1.0.0")
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"revision":1,"models":[{"model":{"slug":"bge-m3","capability":"embedding"},
			"version":{"version":%q,"backend":"tei-embed","min_vram_mb":1024,"recommended_vram_mb":2048}}]}`,
			version.Load().(string))
	}))
	defer catalog.Close()
	tei := teitest.New(teitest.Options{})
	defer tei.Close()

	cfg := &config.Config{
		CatalogURL:           catalog.URL + "/api/catalog",
		CatalogRefreshSecs:   1,
		TEIURL:               tei.URL,
		TEIHealthURL:         tei.URL + "/health",
		ProbeTimeoutMS:       2_000,
		Enabled:              []string{"bge-m3"},
		InferenceTimeoutSecs: 30,
	}
	sup := &supervisor{cfg: cfg, info: gpu.Info{Model: "Test", VRAMMB: 8192}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !sup.rebuild(ctx, true) {
		t.Fatal("initial rebuild failed")
	}
	if got := sup.engine.ModelAdverts(); len(got) != 1 || got[0].Version != "1.0.0" {
		t.Fatalf("initial adverts = %+v", got)
	}

	version.Store("1.0.1")
	connCtx, reconnect := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sup.watch(connCtx, reconnect)
	}()
	select {
	case <-connCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never asked to re-register after the catalog changed")
	}
	<-done
	if got := sup.engine.ModelAdverts(); len(got) != 1 || got[0].Version != "1.0.1" {
		t.Fatalf("adverts after refresh = %+v, want bge-m3@1.0.1", got)
	}
}
