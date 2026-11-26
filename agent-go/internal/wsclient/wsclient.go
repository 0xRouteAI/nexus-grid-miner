// Package wsclient maintains the reverse WebSocket to the server.
package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/engine"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/gpu"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/protocol"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/signing"
	"github.com/gorilla/websocket"
	"log/slog"
)

// readTimeout is how long the socket may go without any inbound frame before
// the client treats it as dead. It is a variable so tests can shrink it; the
// server pings every 20s, so production always refreshes it well within
// the window.
var readTimeout = 90 * time.Second

// Outcome classifies why a connection ended.
type Outcome int

const (
	Disconnected Outcome = iota
	Fatal
)

// Result carries the outcome of one connection attempt.
type Result struct {
	Outcome Outcome
	Reason  string
}

// RunOnce connects, registers, and serves tasks until the socket closes.
//
// The read loop must never stop: gorilla/websocket only dispatches control
// frames (including the server's Ping) while a read is in flight, so a
// write-only client would be reaped as stale after 90s.
func RunOnce(
	ctx context.Context,
	cfg *config.Config,
	info gpu.Info,
	eng *engine.Engine,
) (Result, error) {
	// Sign before dialing: a bad key must fail startup, not every reconnect.
	var proof *signing.Proof
	if cfg.PrivateKey != "" {
		built, err := signing.Build(cfg.PrivateKey, cfg.Wallet, cfg.SignatureDomain, cfg.NodeID)
		if err != nil {
			return Result{}, fmt.Errorf("registration identity: %w", err)
		}
		proof = &built
	} else {
		slog.Warn("MINER_PRIVATE_KEY is not set; registering unsigned")
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   32 * 1024,
		WriteBufferSize:  32 * 1024,
	}

	conn, _, err := dialer.DialContext(ctx, cfg.ServerURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("WebSocket connect failed: %w", err)
	}
	defer conn.Close()

	// The server pings every 20s. Every inbound control frame counts as
	// liveness: without extending the deadline on Ping the read loop times out
	// after 90s even though the socket is healthy, because the server never
	// sends a Pong of its own. gorilla's default ping handler only replies; a
	// custom handler must send the Pong itself.
	extendReadDeadline := func() {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	}
	conn.SetPongHandler(func(string) error {
		extendReadDeadline()
		return nil
	})
	conn.SetPingHandler(func(payload string) error {
		extendReadDeadline()
		return conn.WriteControl(websocket.PongMessage, []byte(payload),
			time.Now().Add(10*time.Second))
	})

	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteJSON(v)
	}

	register := protocol.Register{
		Type:            "register",
		ProtocolVersion: protocol.ProtocolVersion,
		Wallet:          cfg.Wallet,
		GPUModel:        info.Model,
		VRAMMB:          info.VRAMMB,
		Models:          eng.Capabilities(),
		ModelVersions:   eng.ModelAdverts(),
		AgentVersion:    cfg.AgentVersion,
		NodeID:          cfg.NodeID,
	}
	if proof != nil {
		register.NodeID = proof.NodeID
		register.Signature = proof.Signature
		register.IssuedAt = proof.IssuedAt
		register.Challenge = proof.Challenge
	}
	if err := writeJSON(register); err != nil {
		return Result{}, fmt.Errorf("sending register: %w", err)
	}

	slog.Info("connecting to server",
		"server", cfg.ServerURL,
		"wallet", cfg.Wallet,
		"gpu", info.Model,
		"vram_mb", info.VRAMMB,
		"capabilities", eng.Capabilities(),
	)

	// Registration must be acknowledged (or rejected) within the server's 15s.
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	var inflight atomic.Int32
	permits := make(chan struct{}, cfg.MaxConcurrentTasks)

	// Teardown order matters (a send on a closed channel panics the whole
	// agent): the outbox is NEVER closed. Instead connDone is closed once,
	// every sender selects on it, connCtx cancels in-flight inference, and
	// RunOnce waits for task goroutines and the writer before returning.
	connCtx, cancelConn := context.WithCancel(ctx)
	connDone := make(chan struct{})
	var closeOnce sync.Once
	shutdown := func() {
		closeOnce.Do(func() {
			cancelConn()
			close(connDone)
		})
	}
	var tasks sync.WaitGroup

	// Writer goroutine serialises all outbound frames. A channel keeps the
	// heartbeat and task results from interleaving partial frames.
	outbox := make(chan []byte, 128)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-connDone:
				return
			case payload := <-outbox:
				writeMu.Lock()
				_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				err := conn.WriteMessage(websocket.TextMessage, payload)
				writeMu.Unlock()
				if err != nil {
					// Surface the failure to the reader by closing the socket.
					_ = conn.Close()
					return
				}
			}
		}
	}()

	// enqueue never blocks the caller and never panics after shutdown. A full
	// queue means the socket is wedged: close it so the server fails the task
	// over to another node instead of waiting for a result that was dropped.
	enqueue := func(payload []byte, what string) {
		select {
		case <-connDone:
			return
		default:
		}
		select {
		case outbox <- payload:
		case <-connDone:
		default:
			slog.Error("outbound queue full; closing connection", "frame", what)
			_ = conn.Close()
		}
	}
	send := func(v any) {
		payload, err := json.Marshal(v)
		if err != nil {
			return
		}
		enqueue(payload, "control")
	}

	heartbeat := time.NewTicker(time.Duration(cfg.HeartbeatSecs) * time.Second)
	defer heartbeat.Stop()

	go func() {
		for {
			select {
			case <-connDone:
				return
			case <-heartbeat.C:
				send(protocol.Heartbeat{
					Type:     "heartbeat",
					Inflight: inflight.Load(),
					TS:       time.Now().Unix(),
				})
			}
		}
	}()

	defer func() {
		shutdown()
		_ = conn.Close()
		tasks.Wait()
		<-writerDone
	}()

	// A context cancelled from outside (SIGTERM, catalog refresh) closes the
	// socket so the blocking read below returns.
	go func() {
		select {
		case <-connCtx.Done():
			_ = conn.Close()
		case <-connDone:
		}
	}()

	stopReason := ""
	for {
		// Any inbound frame is proof of life; refresh before blocking again so a
		// quiet-but-healthy socket is not mistaken for a dead one.
		extendReadDeadline()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return Result{Outcome: Disconnected, Reason: "closed by server"}, nil
			}
			return Result{Outcome: Disconnected, Reason: err.Error()}, nil
		}

		var msg protocol.ServerMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("ignoring malformed server message", "err", err)
			continue
		}

		switch msg.Type {
		case "registered":
			slog.Info("miner registered with server",
				"reputation", msg.Reputation,
				"heartbeat_secs", msg.HeartbeatSecs,
			)

		case "task":
			inflight.Add(1)
			tasks.Add(1)
			go func(m protocol.ServerMessage) {
				defer tasks.Done()
				defer inflight.Add(-1)

				select {
				case permits <- struct{}{}:
				case <-connDone:
					return
				}
				defer func() { <-permits }()

				result, latency, errMsg := eng.ExecuteRouteContext(connCtx,
					m.Capability, m.Model, m.Version, m.Backend, m.Payload, m.DeadlineMS)

				var reply protocol.ClientResult
				if errMsg != "" {
					slog.Warn("task failed locally",
						"task_id", m.TaskID, "model", m.Model, "version", m.Version,
						"capability", m.Capability, "err", errMsg)
					reply = protocol.Failure(m.TaskID, latency, errMsg)
				} else {
					reply = protocol.Success(m.TaskID, latency, result)
				}

				// Serialise before queueing so a marshal error cannot leave the
				// server waiting for this task id.
				payload, marshalErr := json.Marshal(reply)
				if marshalErr != nil {
					fallback, _ := json.Marshal(protocol.Failure(
						m.TaskID, latency,
						fmt.Sprintf("could not serialize local result: %v", marshalErr),
					))
					payload = fallback
				}
				enqueue(payload, "result:"+m.TaskID)
			}(msg)

		case "error":
			if msg.Fatal {
				slog.Error("server sent a fatal error", "message", msg.Message)
				stopReason = msg.Message
				// Let the deferred writer drain, then report fatal.
				return Result{Outcome: Fatal, Reason: stopReason}, nil
			}
			slog.Warn("server warning", "message", msg.Message)

		case "pong":
			// no-op

		default:
			slog.Debug("ignoring unknown server message", "type", msg.Type)
		}
	}
}
