package config

import (
	"reflect"
	"testing"
)

const testKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func setEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func baseEnv() map[string]string {
	return map[string]string{
		"SERVER_URL":        "nexus.example.com",
		"WALLET":            "0xTEST000000000000000000000000000000000001",
		"MINER_PRIVATE_KEY": testKey,
	}
}

func TestFromEnvParsesAndClamps(t *testing.T) {
	env := baseEnv()
	for k, v := range map[string]string{
		"ENABLED_MODELS":         "embedding,rerank",
		"HEARTBEAT_SECS":         "1",  // clamp to 5
		"MAX_CONCURRENT_TASKS":   "99", // clamp to 32
		"INFERENCE_TIMEOUT_SECS": "1",  // clamp to 10
		"TEI_URL":                "http://tei:80/",
	} {
		env[k] = v
	}
	setEnv(t, env)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ServerURL != "wss://nexus.example.com/ws/miner" {
		t.Fatalf("server url = %q", cfg.ServerURL)
	}
	if cfg.CatalogURL != "https://nexus.example.com/api/catalog" {
		t.Fatalf("catalog url = %q", cfg.CatalogURL)
	}
	if !reflect.DeepEqual(cfg.Enabled, []string{"bge-m3", "bge-reranker-v2-m3"}) {
		t.Fatalf("enabled = %v", cfg.Enabled)
	}
	if cfg.HeartbeatSecs != 5 || cfg.MaxConcurrentTasks != 32 || cfg.InferenceTimeoutSecs != 10 {
		t.Fatalf("clamps: heartbeat=%d concurrent=%d timeout=%d",
			cfg.HeartbeatSecs, cfg.MaxConcurrentTasks, cfg.InferenceTimeoutSecs)
	}
	if cfg.TEIURL != "http://tei:80" || cfg.TEIHealthURL != "http://tei:80/health" {
		t.Fatalf("TEI urls = %q / %q", cfg.TEIURL, cfg.TEIHealthURL)
	}
	if cfg.TEIEmbedURL() != "http://tei:80/embed" {
		t.Fatalf("TEI embed url = %q", cfg.TEIEmbedURL())
	}
}

func TestFromEnvRejectsBadValues(t *testing.T) {
	cases := []map[string]string{
		{"SERVER_URL": ""},
		{"SERVER_URL": "ftp://nexus.example.com"},
		{"WALLET": ""},
		{"MINER_PRIVATE_KEY": ""},
		{"ENABLED_MODELS": "not-a-model"},
		{"ENABLED_MODELS": "bge-m3,kokoro"},
	}
	for _, override := range cases {
		env := baseEnv()
		for k, v := range override {
			env[k] = v
		}
		setEnv(t, env)
		if _, err := FromEnv(); err == nil {
			t.Fatalf("FromEnv accepted invalid config: %v", override)
		}
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"nexus.example.com":                "wss://nexus.example.com/ws/miner",
		"  nexus.example.com/  ":           "wss://nexus.example.com/ws/miner",
		"https://nexus.example.com":        "wss://nexus.example.com/ws/miner",
		"http://10.0.0.5:8000":             "ws://10.0.0.5:8000/ws/miner",
		"wss://nexus.example.com/ws/miner": "wss://nexus.example.com/ws/miner",
		"ws://127.0.0.1:8000/ws/miner/":    "ws://127.0.0.1:8000/ws/miner",
		"https://example.com/grid":         "wss://example.com/grid/ws/miner",
	}
	for in, want := range cases {
		got, err := NormalizeServerURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestParseModelAliases(t *testing.T) {
	models, err := parseModels("BGE, embeddings , bge-m3")
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"bge-m3"}) {
		t.Fatalf("models = %v", models)
	}
	// Models this network does not serve fail loudly with a clear message.
	if _, err := parseModels("bge-m3,kokoro"); err == nil {
		t.Fatal("kokoro must be rejected")
	}

	models, err = parseModels("bge-reranker, reranker, cross-encoder, RERANK")
	if err != nil {
		t.Fatalf("parseModels reranker: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"bge-reranker-v2-m3"}) {
		t.Fatalf("reranker aliases = %v", models)
	}

	if _, err := parseModels("bge-m3,llama"); err == nil {
		t.Fatal("unknown model alias must be rejected")
	}
}

func TestDefaultEnabledModelsAreLaunchModels(t *testing.T) {
	setEnv(t, baseEnv())
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := []string{"bge-m3", "bge-reranker-v2-m3"}
	if !reflect.DeepEqual(cfg.Enabled, want) {
		t.Fatalf("default enabled = %v, want %v", cfg.Enabled, want)
	}
	if cfg.TEIRerankURL() != "http://reranker:80/rerank" {
		t.Fatalf("TEI rerank url = %q", cfg.TEIRerankURL())
	}
}

func TestEnabledModel(t *testing.T) {
	cfg := &Config{Enabled: []string{"bge-m3"}}
	if !cfg.EnabledModel("bge-m3") || cfg.EnabledModel("bge-reranker-v2-m3") {
		t.Fatalf("EnabledModel is wrong for %v", cfg.Enabled)
	}
}
