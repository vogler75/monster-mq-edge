package tlsutil

import (
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandPath(t *testing.T) {
	got := ExpandPath("certs/peer-{NodeId}/{NodeId}.pem", "oa-a")
	if got != "certs/peer-oa-a/oa-a.pem" {
		t.Fatalf("got %q", got)
	}
	if got := ExpandPath("certs/peer.pem", "oa-a"); got != "certs/peer.pem" {
		t.Fatalf("got %q", got)
	}
}

func TestParseEnums(t *testing.T) {
	for in, want := range map[string]ClientAuth{"": ClientAuthNone, "none": ClientAuthNone, " Request ": ClientAuthRequest, "REQUIRED": ClientAuthRequired} {
		if got, err := ParseClientAuth(in); err != nil || got != want {
			t.Errorf("ParseClientAuth(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseClientAuth("OPTIONAL"); err == nil {
		t.Error("ParseClientAuth accepted OPTIONAL")
	}
	for in, want := range map[string]IdentityFallback{"": FallbackNone, "none": FallbackNone, "dns": FallbackDNS, "CN": FallbackCN} {
		if got, err := ParseIdentityFallback(in); err != nil || got != want {
			t.Errorf("ParseIdentityFallback(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseIdentityFallback("SAN"); err == nil {
		t.Error("ParseIdentityFallback accepted SAN")
	}
}

func TestParsePin(t *testing.T) {
	c := nodeCert(t, nil, "oa-a")
	hexPin := SPKIFingerprint(c.cert)
	if len(hexPin) != 64 || hexPin != strings.ToLower(hexPin) {
		t.Fatalf("fingerprint %q", hexPin)
	}
	var colon []string
	for i := 0; i < len(hexPin); i += 2 {
		colon = append(colon, strings.ToUpper(hexPin[i:i+2]))
	}
	for _, s := range []string{hexPin, strings.ToUpper(hexPin), strings.Join(colon, ":"), "  " + hexPin + "\n"} {
		p, err := ParsePin(s)
		if err != nil || p != SPKIPin(c.cert) {
			t.Fatalf("ParsePin(%q) = %v, %v", s, p, err)
		}
	}
	for _, s := range []string{"", hexPin[:62], hexPin + "00", strings.Replace(hexPin, hexPin[:1], "g", 1)} {
		if _, err := ParsePin(s); err == nil {
			t.Fatalf("ParsePin(%q) accepted", s)
		}
	}
	pins, err := ParsePins([]string{hexPin, CertPin(c.cert).String()})
	if err != nil || len(pins) != 2 || !MatchPins(c.cert, pins[1:]) {
		t.Fatalf("ParsePins: %v %v", pins, err)
	}
	if _, err := ParsePins([]string{hexPin, "nope"}); err == nil {
		t.Fatal("ParsePins accepted a bad entry")
	}
	if pins, err := ParsePins(nil); pins != nil || err != nil {
		t.Fatalf("ParsePins(nil) = %v, %v", pins, err)
	}
}

func TestDecodeSecret(t *testing.T) {
	raw := []byte("0123456789abcdef\xff\xfe")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := DecodeSecret(" " + enc.EncodeToString(raw) + " ")
		if err != nil || string(got) != string(raw) {
			t.Fatalf("%v: %q %v", enc, got, err)
		}
	}
	if _, err := DecodeSecret(base64.StdEncoding.EncodeToString([]byte("too short"))); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := DecodeSecret("not base64 at all!"); err == nil {
		t.Fatal("non-base64 accepted")
	}
	good := base64.StdEncoding.EncodeToString(raw)
	list, err := DecodeSecrets([]string{good, good})
	if err != nil || len(list) != 2 {
		t.Fatalf("DecodeSecrets: %v %v", list, err)
	}
	_, err = DecodeSecrets([]string{good, "c2hvcnQ="})
	if err == nil || !strings.Contains(err.Error(), "secret 2") || strings.Contains(err.Error(), "c2hvcnQ") {
		t.Fatalf("error must name the position, not the secret: %v", err)
	}
}

func TestLoadKeyPair(t *testing.T) {
	dir := t.TempDir()
	c := nodeCert(t, newCA(t, "ca"), "oa-a")
	certPath, keyPath := writeKeyPair(t, dir, "peer", c)

	pair, err := LoadKeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if pair.Leaf == nil || !pair.Leaf.Equal(c.cert) {
		t.Fatal("leaf not parsed")
	}
	if _, err := LoadKeyPair(certPath, ""); err == nil {
		t.Fatal("empty key path accepted")
	}
	if _, err := LoadKeyPair(certPath+":"+keyPath, ""); err == nil {
		t.Fatal("cert:key split must not be supported")
	}
	if _, err := LoadKeyPair(certPath, filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("missing key accepted")
	}
	other := nodeCert(t, nil, "oa-b")
	_, otherKey := writeKeyPair(t, dir, "other", other)
	if _, err := LoadKeyPair(certPath, otherKey); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestLoadCertPool(t *testing.T) {
	empty, err := LoadCertPool("", "PKCS12", "")
	if err != nil || empty == nil || !empty.Equal(x509.NewCertPool()) {
		t.Fatalf("no truststore must give an empty pool, got %v %v", empty, err)
	}

	caPEM, err := os.ReadFile("testdata/truststore-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	want := x509.NewCertPool()
	want.AppendCertsFromPEM(caPEM)

	for _, typ := range []string{"", "PEM", "pem"} {
		pool, err := LoadCertPool("testdata/truststore-ca.pem", typ, "")
		if err != nil || !pool.Equal(want) {
			t.Fatalf("PEM %q: %v", typ, err)
		}
	}
	for _, typ := range []string{"PKCS12", "pfx", "P12"} {
		pool, err := LoadCertPool("testdata/truststore-legacy.p12", typ, "changeit")
		if err != nil || !pool.Equal(want) {
			t.Fatalf("PKCS12 %q: %v", typ, err)
		}
	}
	if _, err := LoadCertPool("testdata/truststore-legacy.p12", "PKCS12", "wrong"); err == nil {
		t.Fatal("wrong PKCS12 password accepted")
	}
	// x/crypto/pkcs12 needs a key bag; certificate-only stores fail loudly instead of trusting nothing.
	if _, err := LoadCertPool("testdata/truststore-certonly.p12", "PKCS12", "changeit"); err == nil {
		t.Fatal("certificate-only PKCS12 unexpectedly accepted")
	}
	if _, err := LoadCertPool("testdata/truststore-legacy.p12", "PEM", ""); err == nil {
		t.Fatal("PKCS12 read as PEM accepted")
	}
	if _, err := LoadCertPool("testdata/truststore-ca.pem", "JKS", ""); err == nil {
		t.Fatal("JKS accepted")
	}
	if _, err := LoadCertPool("testdata/missing.pem", "", ""); err == nil {
		t.Fatal("missing truststore accepted")
	}
}
