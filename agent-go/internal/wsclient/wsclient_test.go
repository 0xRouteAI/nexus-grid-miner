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

// TestRunOnceSurvivesIdleReadWindow is the regression test for the 90-second
// reconnect loop: the server pings but never sends a Pong of its own, so a
// client that only refreshes its read deadline on Pong times out on a healthy
// socket. Shrinking readTimeout makes the bug fire in milliseconds.
func TestRunOnceSurvivesIdleReadWindow(t *testing.T) {
	oldTimeout := readTimeout
	readTimeout = 300 * time.Millisecond
	defer func() { readTimeout = oldTimeout }()

	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var register map[string]any
		if err := conn.ReadJSON(&register); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type":           "registered",
			"reputation":     80,
			"heartbeat_secs": 20,
		})

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte("NG"),
					time.Now().Add(time.Second)); err != nil {
					return
				}
			}
		}
	}))
	defer srv.Close()

	tei := teitest.New(teitest.Options{})
	defer tei.Close()
	cfg := &config.Config{
		ServerURL:            "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/miner",
		Wallet:               "0xTEST000000000000000000000000000000000001",
		TEIURL:               tei.URL,
		TEIHealthURL:         tei.URL + "/health",
		ProbeTimeoutMS:       2_000,
		Enabled:              []string{"bge-m3"},
		HeartbeatSecs:        20,
		MaxConcurrentTasks:   4,
		InferenceTimeoutSecs: 150,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng, err := engine.New(ctx, cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	done := make(chan Result, 1)
	go func() {
		result, _ := RunOnce(ctx, cfg, gpu.Info{Model: "Test GPU", VRAMMB: 1024}, eng)
		done <- result
	}()

	var early *Result
	select {
	case result := <-done:
		early = &result
	case <-time.After(5 * readTimeout):
		// Five deadline windows passed with only Ping traffic: the fix holds.
	}

	close(stop)
	if early == nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("RunOnce did not exit after the server closed the socket")
		}
	}
	if early != nil {
		t.Fatalf("client disconnected on a healthy socket: %+v", *early)
	}
}
