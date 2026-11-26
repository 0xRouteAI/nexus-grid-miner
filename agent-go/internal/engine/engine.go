// Package engine routes a server task to a local inference backend.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/catalogclient"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/protocol"
)

// route is one healthy `slug@version` this agent can execute.
type route struct {
	Advert   protocol.ModelAdvert
	Endpoint string
	Health   string
}

// Engine owns the HTTP client and the set of healthy capabilities.
type Engine struct {
	cfg          *config.Config
	client       *http.Client
	capabilities []string
	routes       map[string]route
}

// New probes exact model routes (in real mode) and keeps the healthy ones.
// Adverts normally come from the server catalog; when omitted the agent
// falls back to the launch model selected by MODEL at version 1.0.0.
// An agent with no healthy route is a hard error.
func New(ctx context.Context, cfg *config.Config, adverts ...protocol.ModelAdvert) (*Engine, error) {
	timeoutSecs := cfg.InferenceTimeoutSecs
	if timeoutSecs < 10 {
		timeoutSecs = 10
	}
	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}

	e := &Engine{cfg: cfg, client: client, routes: map[string]route{}}

	if len(adverts) == 0 {
		adverts = catalogclient.DefaultAdverts(cfg.Enabled)
	}

	for _, advert := range adverts {
		advert.Slug = strings.ToLower(strings.TrimSpace(advert.Slug))
		advert.Version = strings.TrimSpace(advert.Version)
		advert.Capability = strings.TrimSpace(advert.Capability)
		advert.Backend = strings.TrimSpace(advert.Backend)
		if advert.Slug == "" || advert.Version == "" || advert.Capability == "" {
			slog.Warn("skipping invalid model advert", "advert", advert)
			continue
		}

		endpoint, healthURL, ok := e.endpointFor(advert)
		if !ok {
			slog.Warn("no endpoint configured for model", "model", advert.Key(),
				"capability", advert.Capability)
			continue
		}
		if !e.probe(ctx, true, healthURL) {
			slog.Warn("model backend health probe failed",
				"model", advert.Key(), "health", healthURL)
			continue
		}
		// Health alone proves the port answers; /info proves it serves the
		// model the catalog expects. Without this an operator could advertise
		// bge-m3 while running a different checkpoint.
		if cfg.RequireModelInfo {
			if err := e.verifyModelInfo(ctx, advert, endpoint); err != nil {
				slog.Warn("model identity check failed",
					"model", advert.Key(), "endpoint", endpoint, "err", err)
				continue
			}
		}
		key := advert.Key()
		if _, duplicate := e.routes[key]; duplicate {
			continue
		}
		e.routes[key] = route{Advert: advert, Endpoint: endpoint, Health: healthURL}
	}

	// Embedding and rerank are advertised only when an exact version is healthy.
	for _, capability := range []string{protocol.CapEmbedding, protocol.CapRerank} {
		if e.capabilityHealthy(capability) {
			e.capabilities = append(e.capabilities, canonicalModelForCapability(capability))
		}
	}

	if len(e.capabilities) == 0 {
		return nil, fmt.Errorf(
			"no inference capability is available: check that the TEI containers are running and " +
				"that MODEL_ENDPOINTS and the health URLs are correct",
		)
	}

	return e, nil
}

// Capabilities returns the legacy canonical model names.
func (e *Engine) Capabilities() []string { return e.capabilities }

// ModelAdverts returns the exact `slug@version` routes this agent serves.
func (e *Engine) ModelAdverts() []protocol.ModelAdvert {
	keys := make([]string, 0, len(e.routes))
	for key := range e.routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]protocol.ModelAdvert, 0, len(keys))
	for _, key := range keys {
		out = append(out, e.routes[key].Advert)
	}
	return out
}

// RoutesHealthy re-probes every advertised route's health endpoint. The supervisor rebuilds the engine (dropping dead routes)
// when this turns false, so a crashed TEI stops receiving work instead of
// failing tasks and losing reputation.
func (e *Engine) RoutesHealthy(ctx context.Context) bool {
	for _, r := range e.routes {
		if r.Health != "" && !e.probe(ctx, true, r.Health) {
			return false
		}
	}
	return true
}

// AdvertSignature is a stable string for "what this engine serves", used to
// detect that a rebuild changed the advert set and a re-registration is due.
func AdvertSignature(adverts []protocol.ModelAdvert) string {
	keys := make([]string, 0, len(adverts))
	for _, advert := range adverts {
		keys = append(keys, advert.Key()+"|"+advert.Capability+"|"+advert.Backend)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (e *Engine) capabilityHealthy(capability string) bool {
	// Legacy deployments and tests may have only the canonical capability
	// list; the exact routes are layered on top rather than replacing it.
	if canonical := canonicalModelForCapability(capability); canonical != "" && e.has(canonical) {
		return true
	}
	for _, candidate := range e.routes {
		if candidate.Advert.Capability == capability {
			return true
		}
	}
	return false
}

// endpointFor maps a route to its backend base URL. MODEL_ENDPOINTS lets one
// GPU host several models behind different TEI containers.
func (e *Engine) endpointFor(advert protocol.ModelAdvert) (string, string, bool) {
	endpoint := ""
	health := ""
	switch advert.Capability {
	case protocol.CapEmbedding:
		endpoint, health = e.cfg.TEIURL, e.cfg.TEIHealthURL
	case protocol.CapRerank:
		endpoint, health = e.cfg.RerankURL, e.cfg.RerankHealthURL
	default:
		return "", "", false
	}
	if override := e.cfg.ModelEndpoints[advert.Slug]; override != "" {
		endpoint = override
		health = override + "/health"
	}
	return endpoint, health, true
}

// verifyModelInfo asks TEI `/info` which checkpoint the endpoint loaded and
// compares it with the catalog's model_ref (or TEI_MODEL_ID / RERANK_MODEL_ID
// when the catalog omits it).
func (e *Engine) verifyModelInfo(ctx context.Context, advert protocol.ModelAdvert, endpoint string) error {
	expected := strings.TrimSpace(advert.ModelRef)
	if expected == "" {
		expected = e.expectedModelRef(advert.Capability)
	}
	if expected == "" {
		return nil // unknown capability: the health probe is the only contract
	}

	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.ProbeTimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
		strings.TrimRight(endpoint, "/")+"/info", nil)
	if err != nil {
		return fmt.Errorf("build TEI /info request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("calling TEI /info: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("reading TEI /info: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("TEI /info returned HTTP %d: %s",
			resp.StatusCode, protocol.Truncate(string(raw), 200))
	}
	var info struct {
		ModelID string `json:"model_id"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("TEI /info is not valid JSON: %w", err)
	}
	got := strings.TrimSpace(info.ModelID)
	if got == "" {
		got = strings.TrimSpace(info.ID)
	}
	if got == "" {
		return fmt.Errorf("TEI /info did not report a model id")
	}
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("TEI serves %q, catalog expects %q", got, expected)
	}
	return nil
}

func (e *Engine) expectedModelRef(capability string) string {
	switch capability {
	case protocol.CapEmbedding:
		return e.cfg.TEIModelID
	case protocol.CapRerank:
		return e.cfg.RerankModelID
	default:
		return ""
	}
}

func (e *Engine) has(model string) bool {
	for _, item := range e.capabilities {
		if item == model {
			return true
		}
	}
	return false
}

// Execute runs one legacy task (capability + optional model, no version). It
// is used when a task carries no version; versioned tasks use ExecuteRoute.
func (e *Engine) Execute(capability, model string, payload json.RawMessage, deadlineMS uint64) (any, uint64, string) {
	return e.ExecuteRouteContext(context.Background(), capability, model, "", "", payload, deadlineMS)
}

// ExecuteRoute is ExecuteRouteContext without a parent context.
func (e *Engine) ExecuteRoute(capability, model, version, backend string, payload json.RawMessage, deadlineMS uint64) (any, uint64, string) {
	return e.ExecuteRouteContext(context.Background(), capability, model, version, backend, payload, deadlineMS)
}

// ExecuteRoute runs one task pinned to an exact `slug@version`. The error
// string is returned verbatim to the server as `result.error`, so it is
// user-visible.
//
// parent is the connection's context: when the socket drops, in-flight local
// inference is cancelled instead of burning GPU time for a result nobody can
// receive.
func (e *Engine) ExecuteRouteContext(parent context.Context, capability, model, version, backend string, payload json.RawMessage, deadlineMS uint64) (any, uint64, string) {
	started := time.Now()

	timeout := time.Duration(e.cfg.InferenceTimeoutSecs) * time.Second
	if deadlineMS > 0 {
		// Leave 1.5s of headroom so the result reaches the server before its
		// own deadline fires; floor at 3s.
		requested := time.Duration(deadlineMS)*time.Millisecond - 1500*time.Millisecond
		if requested < 3*time.Second {
			requested = 3 * time.Second
		}
		timeout = requested
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result, err := e.executeInner(ctx, capability, model, version, backend, payload)
	elapsed := uint64(time.Since(started).Milliseconds())

	if err != nil {
		if parent.Err() != nil {
			return nil, elapsed, "connection closed before the task finished"
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, elapsed, fmt.Sprintf(
				"local inference exceeded %.1fs deadline", timeout.Seconds(),
			)
		}
		return nil, elapsed, err.Error()
	}
	return result, elapsed, ""
}

func (e *Engine) executeInner(ctx context.Context, capability, model, version, backend string, payload json.RawMessage) (any, error) {
	endpoint := ""

	if strings.TrimSpace(version) != "" {
		// Exact route: never guess, never substitute another model.
		key := strings.ToLower(strings.TrimSpace(model)) + "@" + strings.TrimSpace(version)
		candidate, ok := e.routes[key]
		if !ok {
			return nil, fmt.Errorf("model `%s` is not installed on this miner", key)
		}
		if capability != "" && capability != candidate.Advert.Capability {
			return nil, fmt.Errorf("model `%s` serves `%s`, not `%s`",
				key, candidate.Advert.Capability, capability)
		}
		if backend != "" && backend != candidate.Advert.Backend {
			return nil, fmt.Errorf("model `%s` runs backend `%s`, not `%s`",
				key, candidate.Advert.Backend, backend)
		}
		capability = candidate.Advert.Capability
		model = candidate.Advert.Slug
		endpoint = candidate.Endpoint
	} else {
		expected := canonicalModelForCapability(capability)
		if expected == "" {
			return nil, fmt.Errorf("unknown capability `%s`", capability)
		}
		if strings.TrimSpace(model) == "" {
			model = expected
		}
		if !strings.EqualFold(strings.TrimSpace(model), expected) {
			return nil, fmt.Errorf("model `%s` does not serve capability `%s`", model, capability)
		}
		for _, candidate := range e.routes {
			if candidate.Advert.Capability == capability {
				endpoint = candidate.Endpoint
				break
			}
		}
	}

	if !e.capabilityHealthy(capability) {
		return nil, fmt.Errorf("capability `%s` (model `%s`) is not healthy on this miner",
			capability, model)
	}

	switch capability {
	case protocol.CapEmbedding:
		if endpoint == "" {
			endpoint = e.cfg.TEIURL
		}
		return e.embeddingAt(ctx, endpoint, payload)
	case protocol.CapRerank:
		if endpoint == "" {
			endpoint = e.cfg.RerankURL
		}
		return e.rerankAt(ctx, endpoint, payload)
	default:
		return nil, fmt.Errorf("unknown capability `%s`", capability)
	}
}

func canonicalModelForCapability(capability string) string {
	switch capability {
	case protocol.CapEmbedding:
		return protocol.ModelEmbedding
	case protocol.CapRerank:
		return protocol.ModelRerank
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Real backends
// ---------------------------------------------------------------------------

func (e *Engine) embeddingAt(ctx context.Context, endpoint string, payload json.RawMessage) (any, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("invalid task payload: %w", err)
	}
	inputs, err := embeddingInputs(body)
	if err != nil {
		return nil, err
	}

	reqBody, _ := json.Marshal(map[string]any{"inputs": inputs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(endpoint, "/")+"/embed", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling TEI /embed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading TEI response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("TEI returned HTTP %d: %s", resp.StatusCode, protocol.Truncate(string(raw), 500))
	}

	vectors, err := parseEmbeddingResponse(raw)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(inputs) {
		return nil, fmt.Errorf("TEI returned %d vector(s) for %d input(s)", len(vectors), len(inputs))
	}

	return map[string]any{
		"embeddings": vectors,
		"usage":      usageForInputs(inputs),
	}, nil
}

func (e *Engine) rerankAt(ctx context.Context, endpoint string, payload json.RawMessage) (any, error) {
	var body struct {
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("invalid rerank payload: %w", err)
	}
	query := strings.TrimSpace(body.Query)
	if query == "" {
		return nil, fmt.Errorf("rerank payload is missing query")
	}
	if len(body.Documents) == 0 {
		return nil, fmt.Errorf("rerank payload is missing documents")
	}
	if len(body.Documents) > 200 {
		return nil, fmt.Errorf("at most 200 rerank documents are supported")
	}
	for index, doc := range body.Documents {
		if strings.TrimSpace(doc) == "" {
			return nil, fmt.Errorf("rerank document #%d must be a non-empty string", index)
		}
		if len([]rune(doc)) > 8192 {
			return nil, fmt.Errorf("rerank document #%d exceeds 8192 characters", index)
		}
	}

	reqBody, _ := json.Marshal(map[string]any{
		"query":    body.Query,
		"texts":    body.Documents,
		"truncate": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(endpoint, "/")+"/rerank", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling TEI /rerank: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading TEI rerank response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("TEI rerank returned HTTP %d: %s", resp.StatusCode, protocol.Truncate(string(raw), 500))
	}

	results, err := parseRerankResponse(raw, len(body.Documents))
	if err != nil {
		return nil, err
	}
	return map[string]any{"results": results}, nil
}

type teiRerankItem struct {
	Index          int      `json:"index"`
	Score          *float64 `json:"score"`
	RelevanceScore *float64 `json:"relevance_score"`
}

// parseRerankResponse accepts TEI's top-level array or an object wrapper with a
// `results` key, then validates index range, uniqueness and finite scores. The
// server re-validates independently; this only stops obviously broken
// backend output from leaving the machine.
func parseRerankResponse(raw []byte, documentCount int) ([]map[string]any, error) {
	var items []teiRerankItem
	if err := json.Unmarshal(raw, &items); err != nil {
		var wrapper struct {
			Results []teiRerankItem `json:"results"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("TEI rerank response is not a JSON array of results")
		}
		items = wrapper.Results
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("TEI rerank returned no results")
	}
	if len(items) > documentCount {
		return nil, fmt.Errorf("TEI rerank returned %d results for %d documents", len(items), documentCount)
	}

	seen := make(map[int]bool, len(items))
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if item.Index < 0 || item.Index >= documentCount {
			return nil, fmt.Errorf("TEI rerank index %d is out of range", item.Index)
		}
		if seen[item.Index] {
			return nil, fmt.Errorf("TEI rerank returned duplicate index %d", item.Index)
		}
		seen[item.Index] = true

		score := item.Score
		if item.RelevanceScore != nil {
			score = item.RelevanceScore
		}
		if score == nil {
			return nil, fmt.Errorf("TEI rerank result %d has no score", item.Index)
		}
		if isNaN(*score) || isInf(*score) || *score < 0 || *score > 1 {
			return nil, fmt.Errorf("TEI rerank result %d has an invalid score", item.Index)
		}
		out = append(out, map[string]any{"index": item.Index, "relevance_score": *score})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (e *Engine) probe(ctx context.Context, enabled bool, url string) bool {
	if !enabled {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.ProbeTimeoutMS)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		slog.Warn("health probe request build failed", "url", url, "err", err)
		return false
	}
	resp, err := e.client.Do(req)
	if err != nil {
		slog.Warn("health probe failed", "url", url, "err", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("health probe returned non-2xx", "url", url, "status", resp.StatusCode)
		return false
	}
	return true
}

func embeddingInputs(body map[string]any) ([]string, error) {
	raw, ok := body["input"]
	if !ok {
		return nil, fmt.Errorf("embedding payload is missing input")
	}

	switch v := raw.(type) {
	case string:
		return []string{v}, nil
	case []any:
		inputs := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("embedding input must be strings, not token arrays")
			}
			inputs = append(inputs, s)
		}
		if len(inputs) == 0 {
			return nil, fmt.Errorf("embedding input must not be empty")
		}
		if len(inputs) > 64 {
			return nil, fmt.Errorf("at most 64 embedding inputs are supported")
		}
		return inputs, nil
	default:
		return nil, fmt.Errorf("embedding input must be a string or an array of strings")
	}
}

// parseEmbeddingResponse accepts TEI's `[[...]]`, a single `[...]`, or any
// object with an `embeddings` key.
func parseEmbeddingResponse(raw []byte) ([][]float64, error) {
	var direct [][]float64
	if err := json.Unmarshal(raw, &direct); err == nil {
		if err := validateVectors(direct); err != nil {
			return nil, err
		}
		return direct, nil
	}

	var single []float64
	if err := json.Unmarshal(raw, &single); err == nil {
		vectors := [][]float64{single}
		if err := validateVectors(vectors); err != nil {
			return nil, err
		}
		return vectors, nil
	}

	var wrapper struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Embeddings != nil {
		if err := validateVectors(wrapper.Embeddings); err != nil {
			return nil, err
		}
		return wrapper.Embeddings, nil
	}

	return nil, fmt.Errorf("TEI response is missing a numeric embedding array")
}

func validateVectors(vectors [][]float64) error {
	if len(vectors) == 0 {
		return fmt.Errorf("TEI returned zero vectors")
	}
	for _, vector := range vectors {
		if len(vector) == 0 || len(vector) > 8192 {
			return fmt.Errorf("TEI vector has invalid dimensions")
		}
		for _, v := range vector {
			if isNaN(v) || isInf(v) {
				return fmt.Errorf("TEI vector contains a non-finite value")
			}
		}
	}
	return nil
}

func isNaN(v float64) bool { return v != v }
func isInf(v float64) bool {
	return v > 1.7976931348623157e308 || v < -1.7976931348623157e308
}

// usageForInputs estimates token usage (about four characters per token). The
// server bills embeddings by its own count; this is informational.
func usageForInputs(inputs []string) map[string]int64 {
	var tokens int64
	for _, input := range inputs {
		n := int64(len([]rune(input)) / 4)
		if n < 1 {
			n = 1
		}
		tokens += n
	}
	return map[string]int64{"prompt_tokens": tokens, "total_tokens": tokens}
}
