package wsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/engine"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/gpu"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/teitest"
	"github.com/gorilla/websocket"
)

// TestDisconnectWithInflightTasks is the regression test for the agent panic:
// the server floods tasks and drops the socket while they are still running.
// Before the fix a finishing task sent on the closed outbox and crashed the
// process. Run with -race.
func TestDisconnectWithInflightTasks(t *testing.T) {
	for round := 0; round < 20; round++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			var register map[string]any
			if err := conn.ReadJSON(&register); err != nil {
				_ = conn.Close()
				return
			}
			_ = conn.WriteJSON(map[string]any{"type": "registered", "reputation": 80, "heartbeat_secs": 20})
			for i := 0; i < 300; i++ {
				_ = conn.WriteJSON(map[string]any{
					"type": "task", "task_id": "t" + string(rune('a'+i%26)),
					"capability": "embedding", "model": "bge-m3",
					"payload":     map[string]any{"input": []string{"hello", "world"}},
					"deadline_ms": 10000,
				})
			}
			_ = conn.Close() // drop mid-flight
		}))

		tei := teitest.New(teitest.Options{Delay: 5 * time.Millisecond})
		defer tei.Close()
		cfg := &config.Config{
			ServerURL:            "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/miner",
			Wallet:               "0xTEST000000000000000000000000000000000001",
			TEIURL:               tei.URL,
			TEIHealthURL:         tei.URL + "/health",
			ProbeTimeoutMS:       2_000,
			Enabled:              []string{"bge-m3"},
			HeartbeatSecs:        20,
			MaxConcurrentTasks:   2,
			InferenceTimeoutSecs: 30,
		}
		ctx, cancel := context.WithCancel(context.Background())
		eng, err := engine.New(ctx, cfg)
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		done := make(chan Result, 1)
		go func() {
			result, _ := RunOnce(ctx, cfg, gpu.Info{Model: "Test GPU", VRAMMB: 1024}, eng)
			done <- result
		}()
		select {
		case result := <-done:
			if result.Outcome != Disconnected {
				t.Fatalf("round %d outcome = %+v, want Disconnected", round, result)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: RunOnce did not return after the server dropped", round)
		}
		cancel()
		srv.Close()
	}
}
