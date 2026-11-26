// Package protocol is the miner <-> server wire contract.
// Field names must match docs/INTERFACES.md.
package protocol

import "encoding/json"

const (
	CapEmbedding = "embedding"
	CapRerank    = "rerank"

	ModelEmbedding = "bge-m3"
	ModelRerank    = "bge-reranker-v2-m3"
)

// ServerMessage is any frame sent by the server. Unknown fields are
// ignored, so the server can add metadata without breaking older agents.
type ServerMessage struct {
	Type string `json:"type"`

	// type=registered
	ProtocolVersion int `json:"protocol_version"`
	Reputation      int `json:"reputation"`
	HeartbeatSecs   int `json:"heartbeat_secs"`

	// type=task
	TaskID     string          `json:"task_id"`
	Capability string          `json:"capability"`
	Model      string          `json:"model"`
	Version    string          `json:"version"`
	Backend    string          `json:"backend"`
	Payload    json.RawMessage `json:"payload"`
	DeadlineMS uint64          `json:"deadline_ms"`

	// type=error
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`

	// type=pong
	ServerTime int64 `json:"server_time"`
}

// ModelAdvert is one exact `slug@version` the agent is serving.
type ModelAdvert struct {
	Slug       string `json:"slug"`
	Version    string `json:"version"`
	Capability string `json:"capability,omitempty"`
	Backend    string `json:"backend,omitempty"`
	// ModelRef is the backend's own model id (for TEI, what /info reports).
	// It is used for the local identity check and ignored by older servers.
	ModelRef string `json:"model_ref,omitempty"`
}

// Key is the exact route identifier.
func (a ModelAdvert) Key() string { return a.Slug + "@" + a.Version }

// ProtocolVersion is the newest wire version this agent speaks.
const ProtocolVersion = 2

// Register is the first frame the agent sends.
type Register struct {
	Type string `json:"type"`
	// ProtocolVersion is the newest wire version this agent speaks (see
	// docs/INTERFACES.md); the server answers with the negotiated one.
	ProtocolVersion int           `json:"protocol_version"`
	Wallet          string        `json:"wallet"`
	GPUModel        string        `json:"gpu_model"`
	VRAMMB          uint64        `json:"vram_mb"`
	Models          []string      `json:"models"`
	ModelVersions   []ModelAdvert `json:"model_versions,omitempty"`
	AgentVersion    string        `json:"agent_version"`
	NodeID          string        `json:"node_id,omitempty"`

	// Wallet proof, signed with MINER_PRIVATE_KEY on every connect.
	Signature string `json:"signature,omitempty"`
	IssuedAt  string `json:"signature_issued_at,omitempty"`
	Challenge string `json:"signature_challenge,omitempty"`
}

// Heartbeat is sent every HEARTBEAT_SECS.
type Heartbeat struct {
	Type     string `json:"type"`
	Inflight int32  `json:"inflight"`
	TS       int64  `json:"ts"`
}

// ClientResult is the reply to a single task. Result is omitted on failure.
type ClientResult struct {
	Type      string `json:"type"`
	TaskID    string `json:"task_id"`
	OK        bool   `json:"ok"`
	LatencyMS uint64 `json:"latency_ms"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Success builds an ok result.
func Success(taskID string, latencyMS uint64, result any) ClientResult {
	return ClientResult{
		Type:      "result",
		TaskID:    taskID,
		OK:        true,
		LatencyMS: latencyMS,
		Result:    result,
	}
}

// Failure builds an error result.
func Failure(taskID string, latencyMS uint64, errMsg string) ClientResult {
	return ClientResult{
		Type:      "result",
		TaskID:    taskID,
		OK:        false,
		LatencyMS: latencyMS,
		Error:     errMsg,
	}
}

// Truncate shortens a message for logging/returning to the server.
func Truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
