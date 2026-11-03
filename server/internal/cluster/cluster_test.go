package cluster

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTURNCredential(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	user, pass := NewTURNCredential("s3cret", "u:12 x", now, time.Hour)
	if strings.Count(user, ":") != 2 || strings.ContainsAny(user[strings.LastIndex(user, ":")+1:], ": ") {
		t.Fatalf("tag not sanitized: %q", user)
	}
	if pass != TURNPassword("s3cret", user) {
		t.Fatal("password mismatch")
	}
	sec, err := TURNSecretFor(user, []string{"new", "s3cret"}, now)
	if err != nil || sec != "s3cret" {
		t.Fatalf("previous secret should validate: %q %v", sec, err)
	}
	if _, err := TURNSecretFor(user, []string{"other"}, now); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := TURNSecretFor(user, []string{"s3cret"}, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired credential accepted")
	}
	for _, bad := range []string{"", "x", "abc:def:ghi", "1:2"} {
		if _, err := TURNSecretFor(bad, []string{"s3cret"}, now); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSubjectMatch(t *testing.T) {
	cases := []struct {
		pat, subj string
		want      bool
	}{
		{"rhis.dev.*.sig", "rhis.dev.42.sig", true},
		{"rhis.dev.*.sig", "rhis.dev.42.other", false},
		{"rhis.>", "rhis.dev.42.sig", true},
		{"rhis.>", "rhis", false},
		{"a.b", "a.b", true},
		{"a.b", "a.b.c", false},
		{"a.*", "a", false},
	}
	for _, c := range cases {
		if got := subjectMatch(strings.Split(c.pat, "."), strings.Split(c.subj, ".")); got != c.want {
			t.Errorf("%s ~ %s = %v, want %v", c.pat, c.subj, got, c.want)
		}
	}
	for _, bad := range []string{"", "a..b", "a.>.b", "a b"} {
		if validSubject(bad, true) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if validSubject("a.*", false) {
		t.Error("wildcards are not allowed when publishing")
	}
}

func TestLocalBus(t *testing.T) {
	b := NewLocalBus()
	var n atomic.Int32
	sub, err := b.Subscribe("rhis.dev.*.sig", func(m Msg) {
		n.Add(1)
		if m.Reply != nil {
			m.Reply(append([]byte("re:"), m.Data...))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Publish(DeviceSubject("7"), []byte("hi"))
	b.Publish("rhis.dev.7.other", []byte("no"))
	if n.Load() != 1 {
		t.Fatalf("got %d deliveries", n.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := b.Request(ctx, DeviceSubject("8"), []byte("ping"))
	if err != nil || string(r) != "re:ping" {
		t.Fatalf("request: %q %v", r, err)
	}
	sub.Unsubscribe()
	if _, err := b.Request(ctx, DeviceSubject("8"), nil); err != ErrNoResponders {
		t.Fatalf("want ErrNoResponders, got %v", err)
	}
}

func TestIdentity(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadIdentity(dir)
	if err != nil || !validNodeID(a.ID) {
		t.Fatalf("new identity: %+v %v", a, err)
	}
	a.Credential = "rc_x"
	a.Save(dir)
	b, err := LoadIdentity(dir)
	if err != nil || b.ID != a.ID || b.Credential != "rc_x" {
		t.Fatalf("reload: %+v %v", b, err)
	}
}
