package remote

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// sealer encrypts small JSON payloads into opaque URL-safe strings with
// AES-256-GCM. Every token, code and client id this server hands out is a
// sealed payload, so the server keeps no state and survives restarts and
// scale-to-zero. Rotating the key revokes everything.
type sealer struct {
	aead   cipher.AEAD
	macKey []byte
	now    func() time.Time
}

var errBadToken = errors.New("invalid or expired token")

func newSealer(key []byte, now func() time.Time) (*sealer, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("signing key must be at least 32 bytes, got %d", len(key))
	}
	encKey := derive(key, "github.com/shmsi/google-health-mcp/enc")
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead, macKey: derive(key, "github.com/shmsi/google-health-mcp/mac"), now: now}, nil
}

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

type envelope struct {
	Exp  int64           `json:"e,omitempty"`
	Data json.RawMessage `json:"d"`
}

// seal encrypts v. kind is bound as associated data so a value sealed as
// one kind (say an auth code) can't be replayed as another (an access
// token). ttl 0 means no expiry.
func (s *sealer) seal(kind string, v any, ttl time.Duration) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	env := envelope{Data: data}
	if ttl > 0 {
		env.Exp = s.now().Add(ttl).Unix()
	}
	plain, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(kind))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// open decrypts a value sealed as kind into v and returns its expiry.
func (s *sealer) open(kind, token string, v any) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return time.Time{}, errBadToken
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(kind))
	if err != nil {
		return time.Time{}, errBadToken
	}
	var env envelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return time.Time{}, errBadToken
	}
	var exp time.Time
	if env.Exp != 0 {
		exp = time.Unix(env.Exp, 0)
		if !s.now().Before(exp) {
			return time.Time{}, errBadToken
		}
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return time.Time{}, errBadToken
	}
	return exp, nil
}

// mac returns a deterministic secret for msg (used for client secrets).
func (s *sealer) mac(msg string) string {
	m := hmac.New(sha256.New, s.macKey)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *sealer) checkMAC(msg, got string) bool {
	return hmac.Equal([]byte(s.mac(msg)), []byte(got))
}
