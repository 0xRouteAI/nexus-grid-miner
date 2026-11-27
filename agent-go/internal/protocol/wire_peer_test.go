package protocol

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestWireFixturesMatchPeerRepository fails when a local NEXUS-GRID checkout is
// present and its contract testdata/wire/ differs. The two directories must stay
// byte-identical. Public CI cannot clone the private control plane, so this
// test skips there; the control-plane CI clones this repo and diffs instead.
//
// Peer resolution order:
//  1. WIRE_PEER_DIR — path to the other repo's wire/ folder
//  2. NEXUS_GRID_DIR — path to the NEXUS-GRID repo root
//  3. Sibling ../NEXUS-GRID next to this miner checkout
func TestWireFixturesMatchPeerRepository(t *testing.T) {
	peer := resolveControlPlaneWireDir()
	if peer == "" {
		t.Skip("control-plane wire fixtures not found; set NEXUS_GRID_DIR or WIRE_PEER_DIR, or keep ../NEXUS-GRID")
	}

	local := filepath.Join("testdata", "wire")
	assertWireDirsEqual(t, local, peer)
}

func resolveControlPlaneWireDir() string {
	if dir := os.Getenv("WIRE_PEER_DIR"); dir != "" {
		return mustWireDir(dir)
	}
	if root := os.Getenv("NEXUS_GRID_DIR"); root != "" {
		return mustWireDir(filepath.Join(
			root, "server", "go-server", "internal", "contract", "testdata", "wire",
		))
	}
	// protocol → internal → agent-go → nexus-grid-miner → sibling NEXUS-GRID
	return mustWireDir(filepath.Join(
		"..", "..", "..", "..", "NEXUS-GRID",
		"server", "go-server", "internal", "contract", "testdata", "wire",
	))
}

func mustWireDir(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}

func assertWireDirsEqual(t *testing.T, local, peer string) {
	t.Helper()

	localFiles, err := listWireFiles(local)
	if err != nil {
		t.Fatalf("list local wire fixtures: %v", err)
	}
	peerFiles, err := listWireFiles(peer)
	if err != nil {
		t.Fatalf("list peer wire fixtures (%s): %v", peer, err)
	}
	if len(localFiles) == 0 {
		t.Fatal("local testdata/wire is empty")
	}

	localSet := map[string]struct{}{}
	for _, name := range localFiles {
		localSet[name] = struct{}{}
	}
	peerSet := map[string]struct{}{}
	for _, name := range peerFiles {
		peerSet[name] = struct{}{}
	}
	for name := range localSet {
		if _, ok := peerSet[name]; !ok {
			t.Errorf("peer missing %s (expected under %s)", name, peer)
		}
	}
	for name := range peerSet {
		if _, ok := localSet[name]; !ok {
			t.Errorf("local missing %s (present in peer %s)", name, peer)
		}
	}
	if t.Failed() {
		return
	}

	for _, name := range localFiles {
		want, err := os.ReadFile(filepath.Join(local, name))
		if err != nil {
			t.Fatalf("read local %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(peer, name))
		if err != nil {
			t.Fatalf("read peer %s: %v", name, err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs from peer %s\n local (%d bytes) vs peer (%d bytes)",
				name, filepath.Join(peer, name), len(want), len(got))
		}
	}
}

func listWireFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
