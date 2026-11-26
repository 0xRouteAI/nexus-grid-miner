package signing

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestBuildProducesRecoverableSignature(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wallet := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	privateKey := "0x" + hex.EncodeToString(crypto.FromECDSA(key))

	proof, err := Build(privateKey, strings.ToUpper(wallet), "nexus-grid", "host-1:abcd1234")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if proof.NodeID != "host-1:abcd1234" || proof.Challenge == "" {
		t.Fatalf("unexpected proof: %+v", proof)
	}
	issuedAt, err := time.Parse(time.RFC3339, proof.IssuedAt)
	if err != nil {
		t.Fatalf("issued_at: %v", err)
	}

	message := Message("nexus-grid", wallet, proof.NodeID, proof.Challenge, issuedAt)
	sig, err := hex.DecodeString(strings.TrimPrefix(proof.Signature, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(message)), sig)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()); got != wallet {
		t.Fatalf("recovered %s, want %s", got, wallet)
	}
}

func TestBuildRejectsMismatchedWallet(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	privateKey := hex.EncodeToString(crypto.FromECDSA(key))
	if _, err := Build(privateKey,
		"0x0000000000000000000000000000000000000002",
		"nexus-grid", "node-1"); err == nil {
		t.Fatal("Build accepted a key that does not own WALLET")
	}
}
