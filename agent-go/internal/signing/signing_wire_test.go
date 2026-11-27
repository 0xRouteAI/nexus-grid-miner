package signing

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The control plane rebuilds exactly this text (NEXUS-GRID internal/auth), so
// the bytes are a cross-repo contract: testdata/wire/ is byte-identical in both
// repositories and a change to either builder must change both.
func TestMessageMatchesWireFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "wire", "eip191.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	issuedAt := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	got := Message(
		"nexus.example",
		"0x1111111111111111111111111111111111111111",
		"node-1",
		"abc123challenge",
		issuedAt,
	)
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("Message mismatch\n got: %q\nwant: %q", got, want)
	}
	// The payload is signed as-is: a trailing newline would change the hash.
	if bytes.HasSuffix(want, []byte("\n")) {
		t.Fatal("fixture has a trailing newline the builder does not emit")
	}
}
