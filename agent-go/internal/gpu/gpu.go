// Package gpu detects the local accelerator.
package gpu

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/0xRouteAI/nexus-grid-miner/agent-go/internal/config"
)

// Info describes the accelerator advertised at registration.
type Info struct {
	Model  string
	VRAMMB uint64
	Source string
}

// Detect resolves GPU information with this precedence:
// env override -> nvidia-smi -> fallback.
func Detect(cfg *config.Config) Info {
	if cfg.GPUModel != "" {
		var vram uint64
		if cfg.HasVRAM {
			vram = cfg.VRAMMB
		}
		return Info{Model: cfg.GPUModel, VRAMMB: vram, Source: "env"}
	}

	if model, vram, ok := queryNvidiaSmi(); ok {
		if cfg.HasVRAM {
			vram = cfg.VRAMMB
		}
		return Info{Model: model, VRAMMB: vram, Source: "nvidia-smi"}
	}

	return Info{Model: "Unknown GPU", VRAMMB: cfg.VRAMMB, Source: "fallback"}
}

// queryNvidiaSmi runs the standard nvidia-smi query string and parses the
// first GPU line. Returns ok=false when the driver or binary is unavailable.
func queryNvidiaSmi() (model string, vram uint64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(
		ctx,
		"nvidia-smi",
		"--query-gpu=name,memory.total",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return "", 0, false
	}

	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	parts := strings.Split(line, ",")
	if len(parts) == 0 {
		return "", 0, false
	}

	model = strings.TrimSpace(parts[0])
	if model == "" {
		return "", 0, false
	}

	if len(parts) > 1 {
		if parsed, convErr := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64); convErr == nil {
			vram = parsed
		}
	}

	return model, vram, true
}

// Describe is used for the startup log line.
func (i Info) Describe() string {
	return fmt.Sprintf("%s (%d MB, source=%s)", i.Model, i.VRAMMB, i.Source)
}
