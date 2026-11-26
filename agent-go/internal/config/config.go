// Package config parses the agent's environment. Variable names and defaults
// are documented in .env.example and docs/INTERFACES.md.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the fully resolved agent configuration.
type Config struct {
	ServerURL string
	Wallet    string

	GPUModel     string
	VRAMMB       uint64
	HasVRAM      bool
	AgentVersion string
	Enabled      []string

	// Registration identity. NodeID is stable across reconnects; PrivateKey
	// signs every registration and is never logged.
	NodeID          string
	PrivateKey      string
	SignatureDomain string

	HeartbeatSecs        int
	MaxConcurrentTasks   int
	InferenceTimeoutSecs int
	ProbeTimeoutMS       int

	// Catalog discovery. An empty CATALOG_URL still works: the agent falls back
	// to MODEL with version 1.0.0.
	CatalogURL         string
	CatalogRefreshSecs int
	VRAMHeadroomMB     uint64
	RequireModelInfo   bool
	// ModelEndpoints maps a model slug to its TEI base URL, overriding the
	// shared TEI_URL/RERANK_URL. Format: `bge-m3=http://tei:80`.
	ModelEndpoints map[string]string

	TEIURL          string
	TEIHealthURL    string
	TEIModelID      string
	RerankURL       string
	RerankHealthURL string
	RerankModelID   string
}

// FromEnv loads configuration and validates it, returning an error instead of
// exiting so callers can log and shut down cleanly.
func FromEnv() (*Config, error) {
	// One GPU serves exactly one model. ENABLED_MODELS is the old name and is
	// still read so existing .env files keep working.
	rawModel := envOptional("MODEL")
	if rawModel == "" {
		rawModel = envOptional("ENABLED_MODELS")
	}
	if rawModel == "" {
		return nil, fmt.Errorf("MODEL is required: the one model this GPU serves (bge-m3 or bge-reranker-v2-m3)")
	}
	enabled, err := parseModels(rawModel)
	if err != nil {
		return nil, err
	}
	if len(enabled) != 1 {
		return nil, fmt.Errorf("MODEL must name exactly one model (one GPU serves one model), got %v; "+
			"run a separate node for each GPU", enabled)
	}

	modelEndpoints, err := parseModelEndpoints(envOptional("MODEL_ENDPOINTS"))
	if err != nil {
		return nil, err
	}

	serverURL, err := NormalizeServerURL(envOptional("SERVER_URL"))
	if err != nil {
		return nil, err
	}

	wallet := envOptional("WALLET")
	if wallet == "" {
		return nil, fmt.Errorf("WALLET is required (the address that earns points)")
	}
	privateKey := envOptional("MINER_PRIVATE_KEY")
	if privateKey == "" {
		return nil, fmt.Errorf("MINER_PRIVATE_KEY is required (it signs every registration)")
	}
	nodeID := envOptional("NODE_ID")
	if nodeID == "" {
		nodeID = defaultNodeID(wallet)
	}

	headroom := envOptionalUint64("VRAM_HEADROOM_MB")
	if envOptional("VRAM_HEADROOM_MB") == "" {
		headroom = 512
	}

	teiURL := trimURL(envString("TEI_URL", "http://embeddings:80"))
	rerankURL := trimURL(envString("RERANK_URL", "http://reranker:80"))

	cfg := &Config{
		ServerURL:       serverURL,
		Wallet:          wallet,
		NodeID:          nodeID,
		PrivateKey:      privateKey,
		SignatureDomain: envString("MINER_SIGNATURE_DOMAIN", "nexus-grid"),

		CatalogURL:         envString("CATALOG_URL", defaultCatalogURL(serverURL)),
		CatalogRefreshSecs: clampInt(envInt("CATALOG_REFRESH_SECS", 300), 30, 86_400),
		VRAMHeadroomMB:     headroom,
		RequireModelInfo:   envBool("REQUIRE_MODEL_INFO_MATCH", true),
		ModelEndpoints:     modelEndpoints,

		GPUModel:     envOptional("GPU_MODEL"),
		VRAMMB:       envOptionalUint64("VRAM_MB"),
		HasVRAM:      envOptional("VRAM_MB") != "",
		AgentVersion: envString("AGENT_VERSION", "0.1.0"),
		Enabled:      enabled,

		HeartbeatSecs:        clampInt(envInt("HEARTBEAT_SECS", 20), 5, 60),
		MaxConcurrentTasks:   clampInt(envInt("MAX_CONCURRENT_TASKS", 4), 1, 32),
		InferenceTimeoutSecs: clampInt(envInt("INFERENCE_TIMEOUT_SECS", 150), 10, 1_800),
		ProbeTimeoutMS:       clampInt(envInt("PROBE_TIMEOUT_MS", 3_000), 300, 30_000),

		TEIURL:          teiURL,
		TEIHealthURL:    envString("TEI_HEALTH_URL", teiURL+"/health"),
		TEIModelID:      envString("TEI_MODEL_ID", "BAAI/bge-m3"),
		RerankURL:       rerankURL,
		RerankHealthURL: envString("RERANK_HEALTH_URL", rerankURL+"/health"),
		RerankModelID:   envString("RERANK_MODEL_ID", "BAAI/bge-reranker-v2-m3"),
	}

	if strings.TrimSpace(cfg.Wallet) == "" {
		return nil, fmt.Errorf("WALLET must not be empty")
	}

	return cfg, nil
}

// Enabled reports whether a canonical model name is advertised.
func (c *Config) EnabledModel(model string) bool {
	for _, item := range c.Enabled {
		if item == model {
			return true
		}
	}
	return false
}

// TEIEmbedURL is the TEI embedding endpoint.
func (c *Config) TEIEmbedURL() string { return c.TEIURL + "/embed" }

// TEIRerankURL is the TEI cross-encoder endpoint.
func (c *Config) TEIRerankURL() string { return c.RerankURL + "/rerank" }

func trimURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// defaultNodeID gives a stable per-host identity when NODE_ID is not set.
// Container hostnames persist across restarts, so reconnects keep the same
// node identity instead of appearing as a new node.
func defaultNodeID(wallet string) string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "miner"
	}
	suffix := strings.TrimSpace(wallet)
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	if suffix == "" {
		suffix = "node"
	}
	return host + ":" + suffix
}

// parseModels accepts the historical aliases so existing .env files
// keep working, and rejects unknown tokens loudly.
func parseModels(raw string) ([]string, error) {
	var models []string
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		normalized := strings.ToLower(strings.TrimSpace(item))
		if normalized == "" {
			continue
		}
		var canonical string
		switch normalized {
		case "bge", "bge-m3", "embedding", "embeddings":
			canonical = "bge-m3"
		case "bge-reranker-v2-m3", "bge-reranker", "reranker", "rerank", "cross-encoder":
			canonical = "bge-reranker-v2-m3"
		case "kokoro", "tts", "speech", "sdxl", "comfyui", "image", "images":
			return nil, fmt.Errorf("MODEL: %q is not served by this network yet; "+
				"use bge-m3,bge-reranker-v2-m3", normalized)
		default:
			return nil, fmt.Errorf("unknown MODEL %q (use bge-m3 or bge-reranker-v2-m3)", normalized)
		}
		if !seen[canonical] {
			seen[canonical] = true
			models = append(models, canonical)
		}
	}
	return models, nil
}

// parseModelEndpoints accepts `slug=url,slug2=url2`. Slugs are lower-cased;
// URLs must be absolute HTTP(S).
func parseModelEndpoints(raw string) (map[string]string, error) {
	endpoints := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		slug, endpoint, found := strings.Cut(item, "=")
		slug = strings.ToLower(strings.TrimSpace(slug))
		endpoint = trimURL(endpoint)
		if !found || slug == "" || endpoint == "" {
			return nil, fmt.Errorf("MODEL_ENDPOINTS entry %q must be slug=url", item)
		}
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			return nil, fmt.Errorf("MODEL_ENDPOINTS URL for %q must be http(s)", slug)
		}
		endpoints[slug] = endpoint
	}
	return endpoints, nil
}

// defaultCatalogURL turns the WebSocket URL into the public catalog endpoint:
// wss://host/ws/miner -> https://host/api/catalog.
func defaultCatalogURL(serverURL string) string {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return ""
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/ws/miner") + "/api/catalog"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func envOptional(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

func envOptionalUint64(key string) uint64 {
	v, ok := os.LookupEnv(key)
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// minerPath is where every NEXUS GRID server accepts miner connections.
const minerPath = "/ws/miner"

// NormalizeServerURL accepts what a person would type and returns the miner
// WebSocket URL:
//
//	nexus.example.com             -> wss://nexus.example.com/ws/miner
//	https://nexus.example.com     -> wss://nexus.example.com/ws/miner
//	http://10.0.0.5:8000          -> ws://10.0.0.5:8000/ws/miner
//	wss://nexus.example.com/ws/miner (unchanged)
//
// A bare host defaults to TLS. Plain ws:// is only used when asked for.
func NormalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("SERVER_URL is required: the network address, e.g. nexus.example.com")
	}
	if !strings.Contains(raw, "://") {
		raw = "wss://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("SERVER_URL %q is not a valid address", raw)
	}
	switch parsed.Scheme {
	case "wss", "ws":
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("SERVER_URL %q: use a host name or an https:// / wss:// address", raw)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, minerPath) {
		path += minerPath
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
