// SPDX-License-Identifier: AGPL-3.0-only

package agentupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func rel(v, ch string) Release {
	return Release{ID: int64(len(v)), Version: v, Channel: ch, OS: "windows", Arch: "amd64"}
}

var releases = []Release{rel("v0.2.0", "production"), rel("v0.3.0", "production"), rel("v0.4.0-test.5", "test")}

func TestDecideChannels(t *testing.T) {
	p := DefaultPolicy()
	if d := Decide(p, nil, 1, "v0.2.0", "production", releases); d.Release == nil || d.Release.Version != "v0.3.0" {
		t.Fatalf("production: %+v", d)
	}
	if d := Decide(p, nil, 1, "0.2.0", "test", releases); d.Release == nil || d.Release.Version != "v0.4.0-test.5" {
		t.Fatalf("test: %+v", d)
	}
	if d := Decide(p, nil, 1, "v0.3.0", "production", releases); d.Release != nil || d.Reason != "current" {
		t.Fatalf("current: %+v", d)
	}
	// A device on a test build that moved to production keeps its newer version.
	if d := Decide(p, nil, 1, "v0.4.0-test.5", "production", releases); d.Release != nil || d.Reason != "newer_installed" {
		t.Fatalf("no downgrade: %+v", d)
	}
}

func TestDecidePinsAndOverrides(t *testing.T) {
	p := DefaultPolicy()
	p.ProductionVersion = "0.2.0"
	d := Decide(p, nil, 1, "v0.3.0", "production", releases)
	if d.Release == nil || d.Release.Version != "v0.2.0" || !d.AllowDowngrade {
		t.Fatalf("pinned downgrade: %+v", d)
	}
	p = DefaultPolicy()
	if d := Decide(p, &Override{Mode: "off"}, 1, "v0.2.0", "production", releases); d.Release != nil || d.Reason != "customer_off" {
		t.Fatalf("customer off: %+v", d)
	}
	if d := Decide(p, &Override{Mode: "pin", Version: "v0.4.0-test.5"}, 1, "v0.2.0", "production", releases); d.Release == nil || d.Release.Version != "v0.4.0-test.5" {
		t.Fatalf("customer pin: %+v", d)
	}
	if d := Decide(p, &Override{Mode: "pin", Version: "v9.9.9"}, 1, "v0.2.0", "production", releases); d.Release != nil || d.Reason != "no_release" {
		t.Fatalf("missing pin: %+v", d)
	}
	p.Enabled = false
	if d := Decide(p, nil, 1, "v0.2.0", "production", releases); d.Reason != "disabled" {
		t.Fatalf("disabled: %+v", d)
	}
}

func TestRolloutIsStableAndGrows(t *testing.T) {
	p := DefaultPolicy()
	p.RolloutPercent = 20
	in := map[int64]bool{}
	for id := int64(1); id <= 1000; id++ {
		if Decide(p, nil, id, "v0.2.0", "production", releases).Release != nil {
			in[id] = true
		}
	}
	if n := len(in); n < 120 || n > 280 {
		t.Fatalf("20%% rollout reached %d of 1000 devices", n)
	}
	p.RolloutPercent = 50
	for id := range in {
		if Decide(p, nil, id, "v0.2.0", "production", releases).Release == nil {
			t.Fatalf("device %d dropped out when the rollout grew", id)
		}
	}
}

func TestSignVerify(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := NewSigner(priv)
	m := &Manifest{Product: "rhis-agent", Version: "v0.3.0", OS: "windows", Arch: "amd64", Size: 10, SHA256: "ab",
		URL: "https://x/api/agent/v1/files/ab", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), DeviceID: 4}
	sg, err := s.Sign(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(sg, []string{"bm90LWEta2V5", s.PublicKey()}, time.Now())
	if err != nil || got.Version != "v0.3.0" || got.DeviceID != 4 {
		t.Fatalf("verify: %v %+v", err, got)
	}
	if _, err := Verify(sg, []string{s.PublicKey()}, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired manifest accepted")
	}
	// Tampering with the manifest breaks the signature.
	raw, _ := base64.StdEncoding.DecodeString(sg.Manifest)
	raw[len(raw)-2] ^= 1
	bad := &Signed{Manifest: base64.StdEncoding.EncodeToString(raw), Signature: sg.Signature}
	if _, err := Verify(bad, []string{s.PublicKey()}, time.Now()); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(sg, []string{NewSigner(other).PublicKey()}, time.Now()); err == nil {
		t.Fatal("wrong key accepted")
	}
	if len(s.Fingerprint()) != 24 {
		t.Fatalf("fingerprint %q", s.Fingerprint())
	}
}

func TestMergeVersions(t *testing.T) {
	got := mergeVersions([]VersionCount{{Version: "0.2.0", Devices: 1, Online: 1}, {Version: "v0.2.0", Devices: 2}, {Version: "", Devices: 3}})
	if len(got) != 2 || got[0].Version != "v0.2.0" || got[0].Devices != 3 || got[0].Online != 1 || got[1].Devices != 3 {
		t.Fatalf("got %+v", got)
	}
}
