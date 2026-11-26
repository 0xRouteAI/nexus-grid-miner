// Package catalogclient discovers the model catalog the server
// publishes. The agent advertises only versions it can actually serve: present
// in ENABLED_MODELS, registered in the catalog, and small enough for this GPU.
package catalogclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/protocol"
)

const maxCatalogBytes = 1 << 20

// Entry is one purchasable model version.
type Entry struct {
	Slug              string
	Version           string
	Capability        string
	Backend           string
	ModelRef          string
	MinVRAMMB         int
	RecommendedVRAMMB int
}

// Snapshot is one fetched catalog revision.
type Snapshot struct {
	Revision int64
	Entries  []Entry
}

type envelope struct {
	Revision int64 `json:"revision"`
	Models   []struct {
		Model struct {
			Slug       string `json:"slug"`
			Capability string `json:"capability"`
		} `json:"model"`
		Version struct {
			Version           string `json:"version"`
			Backend           string `json:"backend"`
			ModelRef          string `json:"model_ref"`
			MinVRAMMB         int    `json:"min_vram_mb"`
			RecommendedVRAMMB int    `json:"recommended_vram_mb"`
		} `json:"version"`
	} `json:"models"`
}

// Fetch downloads GET /api/catalog.
func Fetch(ctx context.Context, endpoint string) (*Snapshot, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("catalog URL is empty")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build catalog request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	if len(raw) > maxCatalogBytes {
		return nil, fmt.Errorf("catalog response exceeds 1 MB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("catalog returned HTTP %d", resp.StatusCode)
	}

	var body envelope
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("catalog is not valid JSON: %w", err)
	}

	snapshot := &Snapshot{Revision: body.Revision}
	for _, item := range body.Models {
		entry := Entry{
			Slug:              strings.ToLower(strings.TrimSpace(item.Model.Slug)),
			Capability:        strings.TrimSpace(item.Model.Capability),
			Version:           strings.TrimSpace(item.Version.Version),
			Backend:           strings.TrimSpace(item.Version.Backend),
			ModelRef:          strings.TrimSpace(item.Version.ModelRef),
			MinVRAMMB:         item.Version.MinVRAMMB,
			RecommendedVRAMMB: item.Version.RecommendedVRAMMB,
		}
		if entry.Slug == "" || entry.Version == "" || entry.Capability == "" {
			continue
		}
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	return snapshot, nil
}

// AdvertsFor filters the catalog to what this miner can advertise. A zero
// vramMB (detection failed) skips the VRAM gate rather than hiding every model.
func AdvertsFor(entries []Entry, enabled []string, vramMB, headroomMB uint64) []protocol.ModelAdvert {
	allow := make(map[string]bool, len(enabled))
	for _, slug := range enabled {
		allow[strings.ToLower(strings.TrimSpace(slug))] = true
	}

	available := uint64(0)
	if vramMB > 0 {
		if headroomMB < vramMB {
			available = vramMB - headroomMB
		}
	}

	var out []protocol.ModelAdvert
	for _, entry := range entries {
		if len(allow) > 0 && !allow[entry.Slug] {
			continue
		}
		if available > 0 && entry.MinVRAMMB > 0 && uint64(entry.MinVRAMMB) > available {
			continue
		}
		out = append(out, protocol.ModelAdvert{
			Slug:       entry.Slug,
			Version:    entry.Version,
			Capability: entry.Capability,
			Backend:    entry.Backend,
			ModelRef:   entry.ModelRef,
		})
	}
	return out
}

// DefaultAdverts builds launch-model adverts for agents running without a
// reachable catalog. Version 1.0.0 is what the launch catalog uses.
func DefaultAdverts(enabled []string) []protocol.ModelAdvert {
	var out []protocol.ModelAdvert
	for _, slug := range enabled {
		switch strings.ToLower(strings.TrimSpace(slug)) {
		case protocol.ModelEmbedding:
			out = append(out, protocol.ModelAdvert{
				Slug: protocol.ModelEmbedding, Version: "1.0.0",
				Capability: protocol.CapEmbedding, Backend: "tei-embed",
				ModelRef: "BAAI/bge-m3",
			})
		case protocol.ModelRerank:
			out = append(out, protocol.ModelAdvert{
				Slug: protocol.ModelRerank, Version: "1.0.0",
				Capability: protocol.CapRerank, Backend: "tei-rerank",
				ModelRef: "BAAI/bge-reranker-v2-m3",
			})
		}
	}
	return out
}
