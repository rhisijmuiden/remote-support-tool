// SPDX-License-Identifier: AGPL-3.0-only

// Package agentupdate makes the server the central updater of its agents: it keeps
// agent releases, decides per device which version it should run (channel, pinned
// versions, staged rollout) and hands out update manifests signed with the server's
// Ed25519 key. Agents only install an update whose manifest verifies against a key
// embedded in their build.
package agentupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

const keySettings = "agent_update_key"

type storedKey struct {
	Public    string    `json:"public"`
	Private   string    `json:"private"` // sealed with the server secret
	CreatedAt time.Time `json:"created_at"`
}

// Signer holds the Ed25519 key that signs update manifests.
type Signer struct {
	priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// LoadSigner loads the signing key, creating it on first start. The key lives in the
// database (encrypted), so every server of a cluster and a restored backup use the same key.
func LoadSigner(ctx context.Context, store *settings.Store, box *secret.Box) (*Signer, error) {
	var k storedKey
	ok, err := store.Get(ctx, keySettings, &k)
	if err != nil {
		return nil, err
	}
	if !ok {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		sealed, err := box.Seal(base64.StdEncoding.EncodeToString(priv.Seed()))
		if err != nil {
			return nil, err
		}
		k = storedKey{Public: base64.StdEncoding.EncodeToString(pub), Private: sealed, CreatedAt: time.Now().UTC()}
		// Insert only if no other server created a key in the meantime, then read back the winner.
		if _, err := store.DB.Exec(ctx, `INSERT INTO settings(key, value) VALUES($1, $2) ON CONFLICT(key) DO NOTHING`, keySettings, mustJSON(k)); err != nil {
			return nil, err
		}
		if _, err := store.Get(ctx, keySettings, &k); err != nil {
			return nil, err
		}
	}
	seedB64, err := box.Open(k.Private)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("agent update key is corrupt")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if base64.StdEncoding.EncodeToString(pub) != k.Public {
		return nil, errors.New("agent update key does not match its public key")
	}
	return &Signer{priv: priv, Pub: pub}, nil
}

// NewSigner wraps an existing key (tests).
func NewSigner(priv ed25519.PrivateKey) *Signer {
	return &Signer{priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
}

// PublicKey is the base64 public key that goes into client builds.
func (s *Signer) PublicKey() string { return base64.StdEncoding.EncodeToString(s.Pub) }

// Fingerprint is a short, human-comparable form of the public key.
func (s *Signer) Fingerprint() string {
	h := sha256.Sum256(s.Pub)
	x := strings.ToUpper(hex.EncodeToString(h[:10]))
	parts := []string{}
	for i := 0; i < len(x); i += 4 {
		parts = append(parts, x[i:i+4])
	}
	return strings.Join(parts, "-")
}

// Manifest describes one update for one device. Field names are part of the agent contract.
type Manifest struct {
	Product        string    `json:"product"` // "rhis-agent" or "rhis-quick"
	Version        string    `json:"version"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256"`
	URL            string    `json:"url"`
	ReleasedAt     time.Time `json:"released_at"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	DeviceID       int64     `json:"device_id"`
	AllowDowngrade bool      `json:"allow_downgrade"`
}

// Signed is what the update endpoint returns.
type Signed struct {
	Manifest  string `json:"manifest"`  // base64 of the manifest JSON bytes
	Signature string `json:"signature"` // base64 Ed25519 signature over those bytes
}

// Sign serialises m and signs the exact bytes.
func (s *Signer) Sign(m *Manifest) (*Signed, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(s.priv, append([]byte(signContext), b...))
	return &Signed{Manifest: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// signContext is prefixed to the manifest bytes before signing, so the key can never be
// tricked into signing something that also parses as another kind of message.
const signContext = "rhis-agent-update-v1\x00"

// Verify is the reference for the agent: it checks the signature against any of keys
// (base64) and the expiry, and returns the manifest.
func Verify(sg *Signed, keys []string, now time.Time) (*Manifest, error) {
	b, err := base64.StdEncoding.DecodeString(sg.Manifest)
	if err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(sg.Signature)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, append([]byte(signContext), b...), sig) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("signature does not verify")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if now.After(m.ExpiresAt) {
		return nil, errors.New("manifest expired")
	}
	return &m, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
