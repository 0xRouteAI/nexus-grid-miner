package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/protocol"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/teitest"
)

func testRerankEngine(url string) *Engine {
	return &Engine{
		cfg: &config.Config{
			InferenceTimeoutSecs: 10,
			RerankURL:            url,
			RerankHealthURL:      url + "/health",
			Enabled:              []string{protocol.ModelRerank},
		},
		client:       &http.Client{Timeout: 5 * time.Second},
		capabilities: []string{protocol.ModelRerank},
	}
}

func rerankPayload(t *testing.T, query string, documents []string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":     protocol.ModelRerank,
		"query":     query,
		"documents": documents,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rerankResults(t *testing.T, result any) map[int]float64 {
	t.Helper()
	object, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map", result)
	}
	rows, ok := object["results"].([]map[string]any)
	if !ok {
		t.Fatalf("results is %T, want []map[string]any", object["results"])
	}
	out := map[int]float64{}
	for _, row := range rows {
		index, ok := row["index"].(int)
		if !ok {
			t.Fatalf("row index is %T", row["index"])
		}
		score, ok := row["relevance_score"].(float64)
		if !ok {
			t.Fatalf("row score is %T", row["relevance_score"])
		}
		out[index] = score
	}
	return out
}

func modelInfoServer(t *testing.T, modelID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/info":
			_, _ = w.Write([]byte(`{"model_id":"` + modelID + `"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func modelInfoConfig(url string) *config.Config {
	return &config.Config{
		RequireModelInfo:     true,
		ProbeTimeoutMS:       2_000,
		InferenceTimeoutSecs: 10,
		TEIURL:               url,
		TEIHealthURL:         url + "/health",
		TEIModelID:           "BAAI/bge-m3",
		Enabled:              []string{protocol.ModelEmbedding},
	}
}

func embeddingAdvert(ref string) protocol.ModelAdvert {
	return protocol.ModelAdvert{
		Slug: protocol.ModelEmbedding, Version: "1.0.0",
		Capability: protocol.CapEmbedding, Backend: "tei-embed", ModelRef: ref,
	}
}

func TestNewAcceptsMatchingTEIModelInfo(t *testing.T) {
	srv := modelInfoServer(t, "BAAI/bge-m3")
	defer srv.Close()

	eng, err := New(context.Background(), modelInfoConfig(srv.URL), embeddingAdvert("BAAI/bge-m3"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if adverts := eng.ModelAdverts(); len(adverts) != 1 || adverts[0].Key() != "bge-m3@1.0.0" {
		t.Fatalf("adverts = %+v", adverts)
	}
}

func TestNewRejectsMismatchedTEIModelInfo(t *testing.T) {
	srv := modelInfoServer(t, "sentence-transformers/all-MiniLM-L6-v2")
	defer srv.Close()

	if _, err := New(context.Background(), modelInfoConfig(srv.URL), embeddingAdvert("BAAI/bge-m3")); err == nil {
		t.Fatal("New accepted an endpoint serving a different model")
	}
}

func TestRerankRealCallsTEIAndConvertsScores(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		_, _ = w.Write([]byte(`[{"index":1,"score":0.94},{"index":0,"score":0.03}]`))
	}))
	defer srv.Close()

	e := testRerankEngine(srv.URL)
	payload := rerankPayload(t, "capital of France", []string{"Berlin is in Germany.", "Paris is the capital of France."})
	result, err := e.executeInner(context.Background(), protocol.CapRerank, protocol.ModelRerank, "", "", payload)
	if err != nil {
		t.Fatalf("executeInner: %v", err)
	}

	if gotBody["query"] != "capital of France" {
		t.Fatalf("query forwarded = %v", gotBody["query"])
	}
	texts, ok := gotBody["texts"].([]any)
	if !ok || len(texts) != 2 {
		t.Fatalf("texts forwarded = %v", gotBody["texts"])
	}

	scores := rerankResults(t, result)
	if scores[1] != 0.94 || scores[0] != 0.03 {
		t.Fatalf("scores = %v", scores)
	}
}

func TestRerankRealAcceptsWrapperAndRelevanceScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.8}]}`))
	}))
	defer srv.Close()

	e := testRerankEngine(srv.URL)
	result, err := e.executeInner(context.Background(), protocol.CapRerank, protocol.ModelRerank, "", "",
		rerankPayload(t, "q", []string{"a"}))
	if err != nil {
		t.Fatalf("executeInner: %v", err)
	}
	if scores := rerankResults(t, result); scores[0] != 0.8 {
		t.Fatalf("scores = %v", scores)
	}
}

func TestRerankRealRejectsBrokenBackendOutput(t *testing.T) {
	cases := []string{
		`not json`,
		`[]`,
		`[{"index":2,"score":0.5}]`,
		`[{"index":0,"score":0.5},{"index":0,"score":0.4}]`,
		`[{"index":0,"score":1.5}]`,
		`[{"index":0,"score":-0.5}]`,
		`[{"index":0}]`,
		`[{"index":0,"score":0.5},{"index":1,"score":0.4}]`,
	}
	for _, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		e := testRerankEngine(srv.URL)
		_, err := e.executeInner(context.Background(), protocol.CapRerank, protocol.ModelRerank, "", "",
			rerankPayload(t, "q", []string{"a"}))
		srv.Close()
		if err == nil {
			t.Fatalf("body %s: expected an error", body)
		}
	}
}

func TestRerankRealRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	e := testRerankEngine(srv.URL)
	_, err := e.executeInner(context.Background(), protocol.CapRerank, protocol.ModelRerank, "", "",
		rerankPayload(t, "q", []string{"a"}))
	if err == nil {
		t.Fatal("expected an error for HTTP 500")
	}
}

func TestExecuteRejectsMismatchedModel(t *testing.T) {
	e := testRerankEngine("http://127.0.0.1:1")
	_, _, errMsg := e.Execute(protocol.CapRerank, protocol.ModelEmbedding,
		rerankPayload(t, "q", []string{"a"}), 0)
	if errMsg == "" {
		t.Fatal("rerank capability accepted an embedding model")
	}
}

func TestExecuteRouteIsExact(t *testing.T) {
	tei := teitest.New(teitest.Options{})
	defer tei.Close()
	cfg := &config.Config{
		TEIURL:               tei.URL,
		TEIHealthURL:         tei.URL + "/health",
		ProbeTimeoutMS:       2_000,
		InferenceTimeoutSecs: 10,
		Enabled:              []string{protocol.ModelEmbedding},
	}
	e, err := New(context.Background(), cfg, protocol.ModelAdvert{
		Slug: "custom-embed", Version: "2.0.0",
		Capability: protocol.CapEmbedding, Backend: "tei-embed",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adverts := e.ModelAdverts()
	if len(adverts) != 1 || adverts[0].Key() != "custom-embed@2.0.0" {
		t.Fatalf("adverts = %+v", adverts)
	}

	payload := json.RawMessage(`{"model":"custom-embed","input":["hello"]}`)
	if _, _, errMsg := e.ExecuteRoute(protocol.CapEmbedding, "custom-embed", "2.0.0",
		"tei-embed", payload, 0); errMsg != "" {
		t.Fatalf("exact route failed: %s", errMsg)
	}
	if _, _, errMsg := e.ExecuteRoute(protocol.CapEmbedding, "custom-embed", "1.0.0",
		"tei-embed", payload, 0); errMsg == "" {
		t.Fatal("unknown version was accepted")
	}
	if _, _, errMsg := e.ExecuteRoute(protocol.CapRerank, "custom-embed", "2.0.0",
		"tei-embed", payload, 0); errMsg == "" {
		t.Fatal("capability mismatch was accepted")
	}
	if _, _, errMsg := e.ExecuteRoute(protocol.CapEmbedding, "custom-embed", "2.0.0",
		"tei-rerank", payload, 0); errMsg == "" {
		t.Fatal("backend mismatch was accepted")
	}
}
