// Command agent is the NEXUS GRID edge miner agent.
//
// It connects, registers, serves tasks, and reconnects with exponential
// backoff (1s -> 30s, jittered) until a fatal server error or
// SIGINT/SIGTERM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/catalogclient"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/engine"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/gpu"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/protocol"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/signing"
	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/wsclient"
)

// discoverAdverts prefers the live catalog and falls back to the launch models
// only when the catalog cannot be reached. A reachable but empty catalog is an
// operator error, not a reason to guess.
func discoverAdverts(ctx context.Context, cfg *config.Config, info gpu.Info) ([]protocol.ModelAdvert, error) {
	if cfg.CatalogURL == "" {
		return catalogclient.DefaultAdverts(cfg.Enabled), nil
	}
	snapshot, err := catalogclient.Fetch(ctx, cfg.CatalogURL)
	if err != nil {
		slog.Warn("catalog fetch failed; falling back to ENABLED_MODELS",
			"url", cfg.CatalogURL, "err", err)
		return catalogclient.DefaultAdverts(cfg.Enabled), nil
	}
	adverts := catalogclient.AdvertsFor(snapshot.Entries, cfg.Enabled, info.VRAMMB, cfg.VRAMHeadroomMB)
	slog.Info("catalog fetched",
		"revision", snapshot.Revision,
		"entries", len(snapshot.Entries),
		"adverts", len(adverts),
		"vram_mb", info.VRAMMB,
		"headroom_mb", cfg.VRAMHeadroomMB)
	if len(adverts) == 0 {
		return nil, fmt.Errorf(
			"catalog revision %d has no model this GPU can serve (vram=%d, headroom=%d); "+
				"check ENABLED_MODELS and MODEL_ENDPOINTS",
			snapshot.Revision, info.VRAMMB, cfg.VRAMHeadroomMB)
	}
	return adverts, nil
}

func main() {
	initLogging()

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	info := gpu.Detect(cfg)
	slog.Info("NEXUS GRID miner agent starting",
		"gpu", info.Model,
		"vram_mb", info.VRAMMB,
		"source", info.Source,
		"wallet", cfg.Wallet,
		"node_id", cfg.NodeID,
		"signed_registration", cfg.PrivateKey != "",
	)

	// A key that does not belong to WALLET would fail every registration;
	// say so once at startup instead of reconnecting forever.
	if _, err := signing.Build(cfg.PrivateKey, cfg.Wallet, cfg.SignatureDomain, cfg.NodeID); err != nil {
		slog.Error("invalid wallet identity", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := &supervisor{cfg: cfg, info: info}
	// Nodes come and go freely: a backend that is still starting (TEI loads
	// the model for a minute) or an unreachable catalog is retried, never fatal.
	if !sup.rebuild(ctx, true) {
		return
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	fatalBackoff := time.Minute
	const maxFatalBackoff = 10 * time.Minute

	for {
		connectedAt := time.Now()
		connCtx, cancelConn := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			sup.watch(connCtx, cancelConn)
		}()

		result, err := wsclient.RunOnce(connCtx, cfg, info, sup.engine)
		reconnectNow := connCtx.Err() != nil && ctx.Err() == nil // watcher asked
		cancelConn()
		<-watchDone
		if ctx.Err() != nil {
			slog.Info("shutdown signal received")
			return
		}

		wait := backoff + time.Duration(rand.Int63n(1000))*time.Millisecond
		switch {
		case reconnectNow:
			if sup.signature == "" {
				// Every backend died: wait here until one serves again.
				if !sup.rebuild(ctx, true) {
					return
				}
			}
			slog.Info("re-registering with the updated model set",
				"model_versions", len(sup.engine.ModelAdverts()))
			wait = 0
		case err != nil:
			slog.Warn("server connection failed; scheduling reconnect", "err", err)
		case result.Outcome == wsclient.Fatal:
			// A fatal rejection is often a stale advert (the operator retired a
			// version) or a temporary ban/admission failure. Refresh the
			// catalog and retry slowly instead of exiting into a restart loop.
			slog.Error("server rejected this node; refreshing catalog and retrying",
				"reason", result.Reason, "retry_in", fatalBackoff.String())
			changed := sup.rebuild(ctx, true) && sup.changed()
			if changed {
				wait = 0
			} else {
				wait = fatalBackoff
				fatalBackoff *= 2
				if fatalBackoff > maxFatalBackoff {
					fatalBackoff = maxFatalBackoff
				}
			}
		default:
			slog.Warn("server connection closed; scheduling reconnect",
				"reason", result.Reason)
		}

		// A connection that survived a while means the network is healthy;
		// reset the backoff instead of resuming at 30s.
		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
			fatalBackoff = time.Minute
		}

		if wait > 0 {
			slog.Warn("reconnecting after backoff", "wait_secs", wait.Seconds())
			select {
			case <-ctx.Done():
				slog.Info("shutdown signal received")
				return
			case <-time.After(wait):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// supervisor owns the current engine and decides when the advertised model
// set has changed (catalog edits, a backend going down or coming back).
type supervisor struct {
	cfg  *config.Config
	info gpu.Info

	engine     *engine.Engine
	discovered []protocol.ModelAdvert
	signature  string
	previous   string
}

// healthCheckInterval is how often backend health is re-probed while
// connected. It is a variable so tests can shrink it.
var healthCheckInterval = 30 * time.Second

// rebuild discovers adverts from the catalog and builds an engine from the
// healthy subset. With wait=true it retries (with backoff) until it succeeds
// or ctx ends; it returns false only when ctx ended.
func (s *supervisor) rebuild(ctx context.Context, wait bool) bool {
	delay := 5 * time.Second
	for {
		adverts, err := discoverAdverts(ctx, s.cfg, s.info)
		if err == nil {
			s.discovered = adverts
			if eng, buildErr := engine.New(ctx, s.cfg, adverts...); buildErr == nil {
				s.previous = s.signature
				s.engine = eng
				s.signature = engine.AdvertSignature(eng.ModelAdverts())
				slog.Info("agent ready",
					"capabilities", eng.Capabilities(),
					"model_versions", len(eng.ModelAdverts()))
				return true
			} else {
				err = buildErr
			}
		}
		if !wait {
			slog.Warn("model rebuild failed; keeping the current engine", "err", err)
			return false
		}
		slog.Warn("no servable model yet; retrying", "err", err, "retry_in", delay.String())
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if delay *= 2; delay > 2*time.Minute {
			delay = 2 * time.Minute
		}
	}
}

func (s *supervisor) changed() bool { return s.signature != s.previous }

// watch runs while one connection is open. It re-probes backend health and
// periodically re-reads the catalog; when the servable set changes it swaps
// the engine and cancels the connection so the main loop re-registers.
func (s *supervisor) watch(connCtx context.Context, reconnect context.CancelFunc) {
	health := time.NewTicker(healthCheckInterval)
	defer health.Stop()
	catalog := time.NewTicker(time.Duration(s.cfg.CatalogRefreshSecs) * time.Second)
	defer catalog.Stop()
	for {
		select {
		case <-connCtx.Done():
			return
		case <-health.C:
			// A dead route must stop being advertised; a recovered one (fewer
			// routes than discovered) should come back.
			if s.engine.RoutesHealthy(connCtx) && len(s.engine.ModelAdverts()) >= len(s.discovered) {
				continue
			}
			if eng, err := engine.New(connCtx, s.cfg, s.discovered...); err == nil {
				if sig := engine.AdvertSignature(eng.ModelAdverts()); sig != s.signature {
					slog.Warn("backend health changed the servable model set; re-registering",
						"model_versions", len(eng.ModelAdverts()))
					s.previous, s.signature, s.engine = s.signature, sig, eng
					reconnect()
					return
				}
			} else {
				slog.Error("every model backend is unhealthy; disconnecting until one recovers",
					"err", err)
				reconnect()
				// The main loop's rebuild after reconnect waits for recovery.
				s.signature = ""
				return
			}
		case <-catalog.C:
			before := s.signature
			if s.rebuild(connCtx, false) && s.signature != before {
				slog.Info("catalog changed the servable model set; re-registering")
				reconnect()
				return
			}
		}
	}
}

// initLogging emits structured logs. Verbosity is controlled by LOG_LEVEL
// (debug|info|warn|error).
func initLogging() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug", "DEBUG":
		level = slog.LevelDebug
	case "warn", "WARN", "warning":
		level = slog.LevelWarn
	case "error", "ERROR":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
