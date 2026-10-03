package tlsutil

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Pin is a SHA-256 over a certificate's SubjectPublicKeyInfo or over the whole certificate DER.
type Pin [sha256.Size]byte

func (p Pin) String() string { return hex.EncodeToString(p[:]) }

// ParsePin accepts 64 hex digits in any case, optionally separated by colons or spaces
// (the openssl fingerprint format).
func ParsePin(s string) (Pin, error) {
	var p Pin
	clean := strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s))
	if len(clean) != 2*len(p) {
		return p, fmt.Errorf("tlsutil: pin %q: want %d hex digits", s, 2*len(p))
	}
	if _, err := hex.Decode(p[:], []byte(clean)); err != nil {
		return p, fmt.Errorf("tlsutil: pin %q: %w", s, err)
	}
	return p, nil
}

// ParsePins parses a pin list; the list allows rotation (17.5).
func ParsePins(list []string) ([]Pin, error) {
	if len(list) == 0 {
		return nil, nil
	}
	pins := make([]Pin, 0, len(list))
	for _, s := range list {
		p, err := ParsePin(s)
		if err != nil {
			return nil, err
		}
		pins = append(pins, p)
	}
	return pins, nil
}

// SPKIPin returns the SHA-256 over the certificate's SubjectPublicKeyInfo. It survives
// re-issuing a certificate for the same key.
func SPKIPin(c *x509.Certificate) Pin { return sha256.Sum256(c.RawSubjectPublicKeyInfo) }

// CertPin returns the SHA-256 over the certificate DER.
func CertPin(c *x509.Certificate) Pin { return sha256.Sum256(c.Raw) }

// SPKIFingerprint returns SPKIPin as lower-case hex, the form logged at startup and configured
// in PinnedSha256.
func SPKIFingerprint(c *x509.Certificate) string { return SPKIPin(c).String() }

// MatchPins reports whether the SPKI or certificate SHA-256 of c is in pins.
func MatchPins(c *x509.Certificate, pins []Pin) bool {
	spki, whole := SPKIPin(c), CertPin(c)
	for _, p := range pins {
		if p == spki || p == whole {
			return true
		}
	}
	return false
}

// MinSecretBytes is the minimum decoded length of a shared secret (18.4).
const MinSecretBytes = 16

// DecodeSecret decodes a base64 shared secret (standard or URL alphabet, padded or not) and
// checks its length.
func DecodeSecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	var (
		b   []byte
		err error
	)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tlsutil: shared secret is not base64: %w", err)
	}
	if len(b) < MinSecretBytes {
		return nil, fmt.Errorf("tlsutil: shared secret has %d bytes, need at least %d", len(b), MinSecretBytes)
	}
	return b, nil
}

// DecodeSecrets decodes a secret list; the first entry is the current secret, the others are
// still accepted (rotation, 17.5). Errors name the list position, never the secret.
func DecodeSecrets(list []string) ([][]byte, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make([][]byte, 0, len(list))
	for i, s := range list {
		b, err := DecodeSecret(s)
		if err != nil {
			return nil, fmt.Errorf("secret %d: %w", i+1, err)
		}
		out = append(out, b)
	}
	return out, nil
}
