package cluster

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// TURN credentials follow the "TURN REST API" scheme that coturn and pion also use:
// the username carries an expiry time and the password is an HMAC of the username with
// a secret shared between the control servers and the relays. Relays therefore never
// need the database to check a credential.
//
// Username format: <unix expiry>:<key id>:<tag>. The key id names which secret signed
// it, so a secret can be rotated while older credentials stay valid until they expire.

// CredentialTTL is how long a TURN credential handed to a viewer or agent stays valid.
const CredentialTTL = 24 * time.Hour

// KeyID returns the short identifier of a TURN secret.
func KeyID(secret string) string {
	h := sha256.Sum256([]byte("rhis-remote/turn-kid\x00" + secret))
	return hex.EncodeToString(h[:4])
}

// TURNPassword computes the password for username.
func TURNPassword(secret, username string) string {
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// NewTURNCredential creates a username/password pair signed with secret.
func NewTURNCredential(secret, tag string, now time.Time, ttl time.Duration) (user, pass string) {
	tag = strings.Map(func(r rune) rune {
		if r == ':' || r < 0x21 || r > 0x7e {
			return '_'
		}
		return r
	}, tag)
	if len(tag) > 64 {
		tag = tag[:64]
	}
	user = strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + KeyID(secret) + ":" + tag
	return user, TURNPassword(secret, user)
}

var errBadTURNUser = errors.New("invalid TURN username")

// TURNSecretFor checks the expiry in username and returns the matching secret.
func TURNSecretFor(username string, secrets []string, now time.Time) (string, error) {
	parts := strings.SplitN(username, ":", 3)
	if len(parts) != 3 {
		return "", errBadTURNUser
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", errBadTURNUser
	}
	if now.Unix() > exp {
		return "", errors.New("TURN credential expired")
	}
	for _, s := range secrets {
		if s != "" && hmac.Equal([]byte(KeyID(s)), []byte(parts[1])) {
			return s, nil
		}
	}
	return "", errors.New("unknown TURN key")
}
