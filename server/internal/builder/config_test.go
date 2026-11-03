// SPDX-License-Identifier: AGPL-3.0-only

package builder

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// fakePE builds the smallest byte slice our PE reader accepts (PE32+, 16 data directories).
func fakePE(body int) []byte {
	b := make([]byte, 0x40+24+240+body)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	opt := 0x40 + 24
	binary.LittleEndian.PutUint16(b[opt:], 0x20b)
	binary.LittleEndian.PutUint32(b[opt+108:], 16)
	for i := opt + 240; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

// fakeSign appends a certificate table like Authenticode does.
func fakeSign(exe []byte) []byte {
	out := append([]byte(nil), exe...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	off := len(out)
	cert := bytes.Repeat([]byte{0xAB}, 64)
	out = append(out, cert...)
	pos := 0x40 + 24 + 112 + 4*8
	binary.LittleEndian.PutUint32(out[pos:], uint32(off))
	binary.LittleEndian.PutUint32(out[pos+4:], uint32(len(cert)))
	return out
}

var fakePNG = []byte("\x89PNG\r\n\x1a\nfake")

func testConfig() *ClientConfig {
	return &ClientConfig{V: 1, BuildID: 7, ProductName: "RHIS Remote", CompanyName: "RHIS", LogoPNG: fakePNG,
		AccentColor: "#FF6B57", Servers: []string{"https://remote.example.com"}, EnrollToken: "tok",
		Mode: "install", Channel: "production", UpdateKeys: []string{"key"}}
}

func TestEmbedExtractRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 5, 13} {
		exe := fakePE(size)
		out, err := Embed(exe, testConfig())
		if err != nil {
			t.Fatal(err)
		}
		if len(out)%8 != 0 {
			t.Fatalf("trailer not 8-byte aligned: %d", len(out))
		}
		if !bytes.Equal(out[:len(exe)], exe) {
			t.Fatal("original bytes changed")
		}
		c, err := Extract(out)
		if err != nil {
			t.Fatal(err)
		}
		if c.BuildID != 7 || c.Servers[0] != "https://remote.example.com" || !bytes.Equal(c.LogoPNG, fakePNG) {
			t.Fatalf("unexpected config %+v", c)
		}
		// After signing, the trailer still sits right before the certificate table.
		c, err = Extract(fakeSign(out))
		if err != nil || c.EnrollToken != "tok" {
			t.Fatalf("signed extract: %v %+v", err, c)
		}
	}
}

func TestEmbedStripsExistingSignature(t *testing.T) {
	exe := fakePE(3)
	signed := fakeSign(exe)
	out, err := Embed(signed, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if off, size, _, _ := peSecurityDir(out); off != 0 || size != 0 {
		t.Fatal("signature directory not cleared")
	}
	if _, err := Extract(out); err != nil {
		t.Fatal(err)
	}
	if _, err := Embed(out, testConfig()); err == nil {
		t.Fatal("embedding twice should fail")
	}
}

func TestExtractRejects(t *testing.T) {
	if _, err := Extract([]byte("not a pe")); err == nil {
		t.Fatal("want error for non-PE")
	}
	if _, err := Extract(fakePE(10)); err == nil {
		t.Fatal("want error without trailer")
	}
}

func TestValidate(t *testing.T) {
	c := testConfig()
	c.Servers = []string{"http://insecure.example.com"}
	if err := c.Validate(); err == nil || !strings.HasPrefix(err.Error(), "server_not_https") {
		t.Fatalf("http server accepted: %v", err)
	}
	c = testConfig()
	c.LogoPNG = []byte("GIF89a")
	if err := c.Validate(); err == nil {
		t.Fatal("non-PNG logo accepted")
	}
	c = testConfig()
	c.AccentColor = "red"
	if err := c.Validate(); err == nil {
		t.Fatal("bad color accepted")
	}
	c = testConfig()
	c.Mode = "other"
	if err := c.Validate(); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestNormalizeServers(t *testing.T) {
	got, err := NormalizeServers([]string{" Remote.Example.com ", "https://remote.example.com/", "https://b.example.com:8443/base/", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://remote.example.com", "https://b.example.com:8443/base"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, bad := range []string{"ftp://x.example.com", "https://user:pw@x.example.com", "https://x.example.com/?a=1", "ws://x"} {
		if _, err := NormalizeServers([]string{bad}); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, err := NormalizeServers(nil); err == nil {
		t.Fatal("empty list accepted")
	}
}
