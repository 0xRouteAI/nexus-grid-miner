package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures are generated from these constructors and the same directory is
// shipped byte-identically in the control-plane repository, which parses the
// register and result frames with its own contract types.
func readWireFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "wire", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func wireRegister() Register {
	return Register{
		Type:            "register",
		ProtocolVersion: ProtocolVersion,
		Wallet:          "0x1111111111111111111111111111111111111111",
		GPUModel:        "NVIDIA Test",
		VRAMMB:          8192,
		Models:          []string{ModelEmbedding},
		ModelVersions: []ModelAdvert{{
			Slug:       ModelEmbedding,
			Version:    "1.0.0",
			Capability: CapEmbedding,
			Backend:    "tei-embed",
			ModelRef:   "BAAI/bge-m3",
		}},
		AgentVersion: "0.1.0",
		NodeID:       "node-1",
		// Placeholder only: the field name is the contract, the value is not.
		Signature: "0x" + strings.Repeat("00", 65),
		IssuedAt:  time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Challenge: "abc123challenge",
	}
}

func TestMarshalsMatchWireFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		value   any
	}{
		{"register.json", wireRegister()},
		{"heartbeat.json", Heartbeat{Type: "heartbeat", Inflight: 1, TS: 1768500000}},
		{"result_ok.json", Success("task-1", 12, map[string]any{"results": []float64{0.1}})},
		{"result_err.json", Failure("task-1", 12, "tei down")},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			want := readWireFixture(t, tc.fixture)
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s mismatch\n got: %s\nwant: %s", tc.fixture, got, want)
			}
		})
	}
}

// The register frame must keep sending model_ref: the server drops it, but
// agents use it for the local identity check and older servers ignore it.
func TestRegisterFixtureKeepsModelRef(t *testing.T) {
	raw := readWireFixture(t, "register.json")
	if !bytes.Contains(raw, []byte(`"model_ref":"BAAI/bge-m3"`)) {
		t.Fatalf("register frame lost model_ref: %s", raw)
	}
}

func TestParsesTaskFixture(t *testing.T) {
	var frame ServerMessage
	if err := json.Unmarshal(readWireFixture(t, "task.json"), &frame); err != nil {
		t.Fatalf("Unmarshal task: %v", err)
	}
	if frame.Type != "task" {
		t.Fatalf("type = %q", frame.Type)
	}
	if frame.TaskID != "task-1" || frame.Capability != CapEmbedding ||
		frame.Model != ModelEmbedding || frame.Version != "1.0.0" ||
		frame.Backend != "tei-embed" || frame.DeadlineMS != 15000 {
		t.Fatalf("frame = %+v", frame)
	}
	if !bytes.Equal(frame.Payload, json.RawMessage(`{"model":"bge-m3","input":["hello"]}`)) {
		t.Fatalf("payload = %s", frame.Payload)
	}
}
