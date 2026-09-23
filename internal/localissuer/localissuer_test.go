// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package localissuer

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
)

// pkcs8 is a private key as the PEM text an operator sets.
func pkcs8(t *testing.T, key crypto.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// rsa2048 is one RSA key shared by the tests that need any key of the
// size, since generating one is most of this package's test time.
var rsa2048 = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	generate := func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, bits) }
	if bits == 2048 {
		generate = rsa2048
	}
	k, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestParseReadsBothFamilies: a PKCS#8 key on P-256 signs ES256 and an
// RSA key RS256, each named by the first sixteen hexadecimal characters
// of the SHA-256 of its public half's PKIX encoding, and neither renders
// its key material under any verb.
func TestParseReadsBothFamilies(t *testing.T) {
	ec, rs := ecKey(t, elliptic.P256()), rsaKey(t, 2048)
	ecRaw, err := ec.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		key    crypto.Signer
		alg    string
		secret string // a fragment of the private key that may appear nowhere
	}{
		{"an ECDSA key on P-256", ec, ES256, hex.EncodeToString(ecRaw)},
		{"an RSA key", rs, RS256, rs.D.Text(16)[:32]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := pkcs8(t, tc.key)
			k, err := Parse("\n  " + text + "\n\n")
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalPKIXPublicKey(tc.key.Public())
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(der)
			if want := hex.EncodeToString(sum[:])[:16]; k.ID() != want {
				t.Errorf("ID() = %q, want %q", k.ID(), want)
			}
			if k.Algorithm() != tc.alg || k.String() != tc.alg+" key "+k.ID() {
				t.Errorf("Algorithm() = %q, String() = %q", k.Algorithm(), k.String())
			}
			if id, err := KeyID(k.Public()); err != nil || id != k.ID() {
				t.Errorf("KeyID(Public()) = %q, %v, want %q", id, err, k.ID())
			}
			body := strings.Split(text, "\n")[1]
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				got := fmt.Sprintf(verb, k)
				if strings.Contains(got, body) || strings.Contains(strings.ToLower(got), strings.ToLower(tc.secret)) {
					t.Errorf("%s renders key material: %s", verb, got)
				}
			}
		})
	}
}

// TestParseRefusesWhatIsNotTheKey: every value but one unencrypted
// PKCS#8 block of a key the verifier checks is a problem naming what was
// found, and no problem echoes the value.
func TestParseRefusesWhatIsNotTheKey(t *testing.T) {
	good := pkcs8(t, ecKey(t, elliptic.P256()))
	sec1, err := x509.MarshalECPrivateKey(ecKey(t, elliptic.P256()))
	if err != nil {
		t.Fatal(err)
	}
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x25519, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block := func(typ string, headers map[string]string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: typ, Headers: headers, Bytes: der}))
	}
	goodDER, _ := pem.Decode([]byte(good))
	for _, tc := range []struct {
		name, value, want string
	}{
		{"an empty value", "", "is not a PEM encoded private key"},
		{"a value that is not PEM", "not-a-key", "is not a PEM encoded private key"},
		{"text before the block", "key:\n" + good, "is not a PEM encoded private key"},
		{"a block that does not decode", "-----BEGIN PRIVATE KEY-----\nAAAA", "begins a PEM block that does not decode"},
		{"two blocks", good + good, "holds more than one PEM block"},
		{"text after the block", good + "trailing", "holds more than one PEM block or text after the block"},
		{"a SEC 1 block", block("EC PRIVATE KEY", nil, sec1), `holds a PEM block of type "EC PRIVATE KEY"`},
		{"a PKCS#1 block", block("RSA PRIVATE KEY", nil, x509.MarshalPKCS1PrivateKey(rsaKey(t, 2048))), `holds a PEM block of type "RSA PRIVATE KEY"`},
		{"a public key", block("PUBLIC KEY", nil, []byte{1}), `holds a PEM block of type "PUBLIC KEY"`},
		{"an encrypted block", block("PRIVATE KEY", map[string]string{"Proc-Type": "4,ENCRYPTED"}, goodDER.Bytes), "holds a PEM block with headers"},
		{"bytes that are not PKCS#8", block("PRIVATE KEY", nil, []byte("not der")), "holds a PRIVATE KEY block that is not a PKCS#8 private key"},
		{"an ECDSA key on P-384", pkcs8(t, ecKey(t, elliptic.P384())), "holds an ECDSA key on P-384"},
		{"an RSA key below 2048 bits", pkcs8(t, rsaKey(t, 1024)), "holds a 1024-bit RSA key, and an RSA key of the local issuer is at least 2048 bits"},
		{"an Ed25519 key", pkcs8(t, edPriv), "holds an Ed25519 key"},
		{"an X25519 key", pkcs8(t, x25519), "holds a *ecdh.PrivateKey, and the local issuer signs with an ECDSA key on P-256 or an RSA key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := Parse(tc.value)
			if err == nil {
				t.Fatalf("Parse accepted the value as %s", k)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
			if body := strings.Split(good, "\n")[1]; strings.Contains(err.Error(), body) {
				t.Errorf("err = %q echoes the value", err)
			}
		})
	}
}

// TestParseListSplitsOnBlocks: a rotation's keys are PEM blocks
// separated by commas, whitespace, or both, read in order; an empty list
// is no key; a problem names the entry by position.
func TestParseListSplitsOnBlocks(t *testing.T) {
	a, b := pkcs8(t, ecKey(t, elliptic.P256())), pkcs8(t, rsaKey(t, 2048))
	ka, err := Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		want        []string
	}{
		{"empty", "", nil},
		{"blank", " ,\n ", nil},
		{"one block", a, []string{ka.ID()}},
		{"a comma between", strings.TrimSpace(a) + "," + strings.TrimSpace(b), []string{ka.ID(), kb.ID()}},
		{"a comma and a newline between", a + ",\n" + b, []string{ka.ID(), kb.ID()}},
		{"blocks one after another", a + b, []string{ka.ID(), kb.ID()}},
		{"a trailing comma", a + ",", []string{ka.ID()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := ParseList(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, k := range keys {
				got = append(got, k.ID())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ids %v, want %v", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, value, want string
	}{
		{"text between the blocks", a + "; " + b, "entry 2 is not a PEM encoded private key"},
		{"an END line that is not closed", a + ",-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY", "entry 2 begins a PEM block that does not decode"},
		{"a second block of another type", a + "," + string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}})), `entry 2 holds a PEM block of type "PUBLIC KEY"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseList(tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMintedTokenVerifies is the signer against the one verifier that
// reads it: a token of either family, signed here, verifies through
// authkit/jwt's local issuer under the key id Parse named, which holds
// ES256 to the fixed-width r||s and the kid rule to the verifier's
// selection. The header and the claims are what the design names, the
// audience a single string and no nbf, and two tokens carry two jtis.
func TestMintedTokenVerifies(t *testing.T) {
	const iss = "https://lux.example.com"
	for _, key := range []crypto.PrivateKey{ecKey(t, elliptic.P256()), rsaKey(t, 2048)} {
		k, err := Parse(pkcs8(t, key))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(k.Algorithm(), func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			claims := Claims{Issuer: iss, Subject: "alice", Audience: "lux", IssuedAt: now, TTL: time.Hour}
			token, err := k.Mint(claims)
			if err != nil {
				t.Fatal(err)
			}
			v := jwt.New(jwt.Config{LocalIssuer: iss, LocalKeys: []jwt.LocalKey{{KeyID: k.ID(), Key: k.Public()}}, Audiences: []string{"lux"}})
			c, err := v.Validate(token)
			if err != nil {
				t.Fatalf("the verifier refused the minted token: %v", err)
			}
			if c.Sub != "alice" || c.Iss != iss || !c.Exp.Equal(now.Add(time.Hour)) {
				t.Errorf("claims = %+v", c)
			}

			parts := strings.Split(token, ".")
			var head map[string]any
			decodeSegment(t, parts[0], &head)
			if len(head) != 3 || head["alg"] != k.Algorithm() || head["typ"] != "JWT" || head["kid"] != k.ID() {
				t.Errorf("header = %v", head)
			}
			var body map[string]any
			decodeSegment(t, parts[1], &body)
			if body["aud"] != "lux" || body["iat"] != float64(now.Unix()) || body["exp"] != float64(now.Add(time.Hour).Unix()) || body["jti"] == "" {
				t.Errorf("payload = %v", body)
			}
			if _, has := body["nbf"]; has {
				t.Errorf("payload carries nbf: %v", body)
			}
			if k.Algorithm() == ES256 {
				sig, err := base64.RawURLEncoding.DecodeString(parts[2])
				if err != nil || len(sig) != 64 {
					t.Errorf("an ES256 signature is %d bytes, want the 64 of r||s: %v", len(sig), err)
				}
			}
			again, err := k.Mint(claims)
			if err != nil {
				t.Fatal(err)
			}
			var second map[string]any
			decodeSegment(t, strings.Split(again, ".")[1], &second)
			if second["jti"] == body["jti"] {
				t.Errorf("two tokens share jti %v", body["jti"])
			}

			other := jwt.New(jwt.Config{LocalIssuer: iss, LocalKeys: []jwt.LocalKey{{KeyID: "0000000000000000", Key: k.Public()}}, Audiences: []string{"lux"}})
			if _, err := other.Validate(token); !errors.Is(err, jwt.ErrUnknownKey) {
				t.Errorf("a verifier holding the key under another id answered %v, want %v", err, jwt.ErrUnknownKey)
			}
		})
	}
}

func decodeSegment(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// TestMintRefusesATokenTheVerifierWould: no issuer, no subject, no
// audience, and a lifetime that is not above zero each refuse to sign.
func TestMintRefusesATokenTheVerifierWould(t *testing.T) {
	k, err := Parse(pkcs8(t, ecKey(t, elliptic.P256())))
	if err != nil {
		t.Fatal(err)
	}
	full := Claims{Issuer: "https://lux.example.com", Subject: "alice", Audience: "lux", IssuedAt: time.Now(), TTL: time.Minute}
	for name, edit := range map[string]func(*Claims){
		"no issuer":      func(c *Claims) { c.Issuer = "" },
		"no subject":     func(c *Claims) { c.Subject = "" },
		"no audience":    func(c *Claims) { c.Audience = "" },
		"a zero TTL":     func(c *Claims) { c.TTL = 0 },
		"a negative TTL": func(c *Claims) { c.TTL = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			c := full
			edit(&c)
			if token, err := k.Mint(c); err == nil {
				t.Fatalf("Mint signed %s", token)
			}
		})
	}
}

// TestKeyIDRefusesAKeyWithNoPKIXForm: a value that is no public key has
// no id.
func TestKeyIDRefusesAKeyWithNoPKIXForm(t *testing.T) {
	if id, err := KeyID("not a key"); err == nil {
		t.Fatalf("KeyID named a string %q", id)
	}
}
