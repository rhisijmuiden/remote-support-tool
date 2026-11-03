// SPDX-License-Identifier: AGPL-3.0-only

package agentupdate

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const policySettings = "agent_update_policy"

// Policy is the global rule for which agent version devices should run.
type Policy struct {
	Enabled bool `json:"enabled"`
	// Pinned versions per channel; empty means "the newest release of the channel".
	ProductionVersion string `json:"production_version"`
	TestVersion       string `json:"test_version"`
	// RolloutPercent offers a new version to this share of devices first (1-100).
	// Each device has a stable bucket, so raising the percentage only adds devices.
	RolloutPercent int `json:"rollout_percent"`
}

func DefaultPolicy() Policy { return Policy{Enabled: true, RolloutPercent: 100} }

// Override is a per-customer exception: "off" stops updates, "pin" holds a version.
type Override struct {
	CustomerID int64     `json:"customer_id"`
	Customer   string    `json:"customer"`
	Mode       string    `json:"mode"`
	Version    string    `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Release is one executable the server can offer.
type Release struct {
	ID         int64     `json:"id"`
	Product    string    `json:"product"`
	Version    string    `json:"version"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	Channel    string    `json:"channel"`
	Size       int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	Source     string    `json:"source"`
	Notes      string    `json:"notes"`
	ReleasedAt time.Time `json:"released_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// CanonVersion returns v with a leading "v", or "" when it is not semantic versioning.
func CanonVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// Decision is the outcome of Decide.
type Decision struct {
	Release        *Release
	AllowDowngrade bool
	Reason         string // why there is no update: disabled, customer_off, current, no_release, rollout, newer_installed
}

// Bucket puts a device in 0..99 for a version, stable across calls.
func Bucket(deviceID int64, version string) int {
	h := fnv.New32a()
	h.Write([]byte(strconv.FormatInt(deviceID, 10) + "/" + version))
	return int(h.Sum32() % 100)
}

// Decide picks the release a device should move to. releases must already be filtered
// to the device's product, OS and architecture.
func Decide(p Policy, o *Override, deviceID int64, current, channel string, releases []Release) Decision {
	if !p.Enabled {
		return Decision{Reason: "disabled"}
	}
	if o != nil && o.Mode == "off" {
		return Decision{Reason: "customer_off"}
	}
	current = CanonVersion(current)
	pin := p.ProductionVersion
	if channel == "test" {
		pin = p.TestVersion
	}
	customerPin := false
	if o != nil && o.Mode == "pin" && o.Version != "" {
		pin, customerPin = o.Version, true
	}
	pin = CanonVersion(pin)

	var target *Release
	for i := range releases {
		r := &releases[i]
		if pin != "" {
			if CanonVersion(r.Version) == pin {
				target = r
				break
			}
			continue
		}
		// Production devices only take production releases; test devices take both.
		if channel != "test" && r.Channel != "production" {
			continue
		}
		if target == nil || semver.Compare(CanonVersion(r.Version), CanonVersion(target.Version)) > 0 {
			target = r
		}
	}
	if target == nil {
		return Decision{Reason: "no_release"}
	}
	tv := CanonVersion(target.Version)
	cmp := semver.Compare(tv, current)
	if current != "" && cmp == 0 {
		return Decision{Reason: "current"}
	}
	// The newest-release rule never downgrades; a pinned version may.
	if current != "" && cmp < 0 && pin == "" {
		return Decision{Reason: "newer_installed"}
	}
	if !customerPin && p.RolloutPercent > 0 && p.RolloutPercent < 100 && Bucket(deviceID, tv) >= p.RolloutPercent {
		return Decision{Reason: "rollout"}
	}
	return Decision{Release: target, AllowDowngrade: current != "" && cmp < 0}
}
