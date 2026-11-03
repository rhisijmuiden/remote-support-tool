package secret

import "testing"

func TestRoundTrip(t *testing.T) {
	b, _ := New("0123456789abcdef0123456789abcdef")
	s, err := b.Seal("hunter2")
	if err != nil || s == "hunter2" {
		t.Fatal(s, err)
	}
	p, err := b.Open(s)
	if err != nil || p != "hunter2" {
		t.Fatal(p, err)
	}
	other, _ := New("another-key-another-key-another-key")
	if _, err := other.Open(s); err == nil {
		t.Fatal("expected error with wrong key")
	}
}
