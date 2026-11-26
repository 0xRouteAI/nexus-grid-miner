// Package teitest is an in-process stand-in for a text-embeddings-inference
// server, used by tests that need a healthy backend without a GPU.
package teitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"
)

// Options tune the fake server.
type Options struct {
	// ModelID is what GET /info reports.
	ModelID string
	// Delay is added to every /embed and /rerank call.
	Delay time.Duration
}

// New starts a fake TEI server. Close it with t.Cleanup or defer.
func New(opts Options) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{}`))
		case "/info":
			_ = json.NewEncoder(w).Encode(map[string]string{"model_id": opts.ModelID})
		case "/embed":
			var body struct {
				Inputs []string `json:"inputs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			time.Sleep(opts.Delay)
			vectors := make([][]float64, len(body.Inputs))
			for i := range vectors {
				vectors[i] = []float64{1, 0, 0, 0}
			}
			_ = json.NewEncoder(w).Encode(vectors)
		case "/rerank":
			var body struct {
				Texts []string `json:"texts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			time.Sleep(opts.Delay)
			out := make([]map[string]any, len(body.Texts))
			for i := range out {
				out[i] = map[string]any{"index": i, "score": 1 / float64(i+2)}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
}
