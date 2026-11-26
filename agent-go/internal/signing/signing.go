// Package signing proves a miner owns the wallet it registers with.
//
// The payload is a domain-separated EIP-191 message, the same one the control
// plane rebuilds in internal/auth. The challenge is generated per connection,
// and the server also bounds issued_at, so a captured frame is only
// replayable inside that window — and replaying it can only impersonate the
// same wallet, which cannot redirect the wallet's points.
package signing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Proof is the registration identity sent in the register frame.
type Proof struct {
	NodeID    string
	Signature string
	IssuedAt  string
	Challenge string
}

// Message builds the exact EIP-191 payload. The server verifies this text byte
// for byte; changing it breaks every deployed agent (see docs/INTERFACES.md).
func Message(domain, wallet, nodeID, challenge string, issuedAt time.Time) string {
	return fmt.Sprintf(
		"NEXUS GRID miner registration\nDomain: %s\nWallet: %s\nNode ID: %s\nChallenge: %s\nIssued At: %s",
		domain, wallet, nodeID, challenge, issuedAt.UTC().Format(time.RFC3339),
	)
}

// NewChallenge returns 16 random bytes as hex.
func NewChallenge() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate challenge: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Build validates the private key against the configured wallet and signs a
// fresh registration proof.
func Build(privateKeyHex, wallet, domain, nodeID string) (Proof, error) {
	keyHex := strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x")
	if keyHex == "" {
		return Proof{}, fmt.Errorf("MINER_PRIVATE_KEY is empty")
	}
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return Proof{}, fmt.Errorf("MINER_PRIVATE_KEY is not a valid secp256k1 key: %w", err)
	}
	derived := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	if !common.IsHexAddress(wallet) {
		return Proof{}, fmt.Errorf("WALLET %q is not a valid address", wallet)
	}
	expected := strings.ToLower(common.HexToAddress(wallet).Hex())
	if derived != expected {
		return Proof{}, fmt.Errorf(
			"MINER_PRIVATE_KEY belongs to %s, not WALLET %s", derived, expected)
	}
	if strings.TrimSpace(domain) == "" {
		return Proof{}, fmt.Errorf("MINER_SIGNATURE_DOMAIN is empty")
	}
	if strings.TrimSpace(nodeID) == "" {
		return Proof{}, fmt.Errorf("NODE_ID is empty")
	}

	challenge, err := NewChallenge()
	if err != nil {
		return Proof{}, err
	}
	issuedAt := time.Now().UTC().Truncate(time.Second)
	message := Message(domain, derived, nodeID, challenge, issuedAt)
	signature, err := crypto.Sign(accounts.TextHash([]byte(message)), key)
	if err != nil {
		return Proof{}, fmt.Errorf("sign registration: %w", err)
	}
	// Personal-sign wallets and the server both accept 27/28; emitting
	// the wallet-style id keeps captured frames compatible with ethers tooling.
	signature[64] += 27

	return Proof{
		NodeID:    nodeID,
		Signature: "0x" + hex.EncodeToString(signature),
		IssuedAt:  issuedAt.Format(time.RFC3339),
		Challenge: challenge,
	}, nil
}
