// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package localissuer is the key of the local issuer of spec 035: the
// private key an operator holds in LUX_LOCAL_ISSUER_KEY, read from its PEM
// form, named by a key id derived from its public half, and used to sign
// the control plane tokens luxd token mints. The verifier checks those
// tokens against the public halves through latere.ai/x/pkg/authkit/jwt,
// so the signature and the header written here are the two that package
// reads: ES256 as the 64 bytes r||s, RS256 as PKCS#1 v1.5 over SHA-256,
// and a kid computed by the one rule KeyID states.
//
// The package reaches the standard library alone: it dials nothing, opens
// nothing, and holds no verifier, so the token role that imports it stays
// a process that reads its configuration and writes one line.
package localissuer

import (
	"crypto"
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
	"time"
)

// The two algorithms, one per key family, the two the shared verifier
// checks: ES256 over an ECDSA key on P-256 and RS256 over an RSA key.
const (
	ES256 = "ES256"
	RS256 = "RS256"
)

// MinRSABits is the smallest RSA modulus a key may have. A smaller key
// signs a credential equivalent to the strongest one the installation
// holds, so it is refused rather than warned about.
const MinRSABits = 2048

// pemType is the one PEM block type read: PKCS#8, which carries either
// key family in one encoding and is what openssl genpkey writes.
const pemType = "PRIVATE KEY"

// Key is one parsed private key of the local issuer: the private key of
// its family, the algorithm that family signs with, and the key id tokens
// name it by. It renders as its algorithm and key id alone, under %v and
// %#v both, so a configuration printed whole carries no key material.
type Key struct {
	ec  *ecdsa.PrivateKey // set for ES256
	rsa *rsa.PrivateKey   // set for RS256
	alg string
	id  string
}

// Parse reads one PEM encoded PKCS#8 private key, an ECDSA key on P-256
// or an RSA key of at least MinRSABits. The value may carry surrounding
// whitespace and nothing else: a second block, text before or after the
// block, a block of another type, and an encrypted block are each
// refused. The error names what was found and never echoes the value;
// the caller prefixes the variable it read.
func Parse(text string) (*Key, error) {
	block, rest, err := decode(text)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rest) != "" {
		return nil, errors.New("holds more than one PEM block or text after the block, and the local issuer's key is one block")
	}
	return fromBlock(block)
}

// ParseList reads the further keys of a rotation: PEM blocks, each as
// Parse reads one, separated by commas, whitespace, or both. A block runs
// to the dashes that close its END line, so a comma may follow them
// directly, which is what "$(cat a.pem),$(cat b.pem)" writes, and a list
// written one block per line is read the same way. A PEM block carries no
// comma, since its body is base64 and a PKCS#8 block has no headers, so
// a comma is always a separator and never part of a key. A problem names
// the failing entry by its position, one-based.
func ParseList(text string) ([]*Key, error) {
	var keys []*Key
	rest := text
	for {
		rest = strings.TrimLeft(rest, ", \t\r\n")
		if rest == "" {
			return keys, nil
		}
		one, after := rest, ""
		if end := strings.Index(rest, "-----END "); end >= 0 {
			if close := strings.Index(rest[end+len("-----END "):], "-----"); close >= 0 {
				cut := end + len("-----END ") + close + len("-----")
				one, after = rest[:cut], rest[cut:]
			}
		}
		k, err := Parse(one)
		if err != nil {
			return nil, fmt.Errorf("entry %d %w", len(keys)+1, err)
		}
		keys = append(keys, k)
		rest = after
	}
}

// decode reads the PEM block text starts with. pem.Decode skips any text
// before a block, so the start is checked here: a value that does not
// begin with a block is refused rather than read from wherever a block
// happens to appear in it.
func decode(text string) (*pem.Block, string, error) {
	text = strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(text, "-----BEGIN ") {
		return nil, "", errors.New("is not a PEM encoded private key (the value is not echoed)")
	}
	block, rest := pem.Decode([]byte(text))
	if block == nil {
		return nil, "", errors.New("begins a PEM block that does not decode (the value is not echoed)")
	}
	return block, string(rest), nil
}

// fromBlock reads one decoded block as the local issuer's key.
func fromBlock(block *pem.Block) (*Key, error) {
	if block.Type != pemType {
		return nil, fmt.Errorf("holds a PEM block of type %q, and the key is read as PKCS#8, type %q, which openssl pkcs8 -topk8 -nocrypt converts it to", block.Type, pemType)
	}
	if len(block.Headers) > 0 {
		return nil, errors.New("holds a PEM block with headers, which an encrypted key carries, and the key is read unencrypted")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("holds a %s block that is not a PKCS#8 private key: %w", pemType, err)
	}
	switch k := parsed.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() {
			return nil, fmt.Errorf("holds an ECDSA key on %s, and an ECDSA key of the local issuer is on P-256", k.Curve.Params().Name)
		}
		return named(&Key{ec: k, alg: ES256})
	case *rsa.PrivateKey:
		if bits := k.N.BitLen(); bits < MinRSABits {
			return nil, fmt.Errorf("holds a %d-bit RSA key, and an RSA key of the local issuer is at least %d bits", bits, MinRSABits)
		}
		return named(&Key{rsa: k, alg: RS256})
	case ed25519.PrivateKey:
		return nil, errors.New("holds an Ed25519 key, and the local issuer signs with an ECDSA key on P-256 or an RSA key")
	default:
		return nil, fmt.Errorf("holds a %T, and the local issuer signs with an ECDSA key on P-256 or an RSA key", parsed)
	}
}

// named sets the key id of a parsed key from its public half.
func named(k *Key) (*Key, error) {
	id, err := KeyID(k.Public())
	if err != nil {
		return nil, err
	}
	k.id = id
	return k, nil
}

// KeyID is the kid of a public key: the first sixteen hexadecimal
// characters of the SHA-256 of its PKIX DER encoding. It depends on the
// public half alone, so the key that signs a token and the key the
// verifier holds for it carry the same id without either being told,
// and a rotation's two keys are told apart by it.
func KeyID(public crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return "", fmt.Errorf("the public key does not encode as PKIX: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16], nil
}

// Algorithm is the JWS alg the key signs with: ES256 or RS256.
func (k *Key) Algorithm() string { return k.alg }

// ID is the key id, the kid of every token the key signs.
func (k *Key) ID() string { return k.id }

// Public is the public half, the one the verifier is handed: an
// *ecdsa.PublicKey for ES256 and an *rsa.PublicKey for RS256.
func (k *Key) Public() crypto.PublicKey {
	if k.ec != nil {
		return &k.ec.PublicKey
	}
	return &k.rsa.PublicKey
}

// String is the key as a line may name it: its algorithm and key id.
func (k *Key) String() string { return k.alg + " key " + k.id }

// GoString is String, so %#v prints no key material either.
func (k *Key) GoString() string { return k.String() }

// Claims is what one token says. Issuer is the local issuer's name,
// LUX_PUBLIC_URL; Subject is the sub the caller renders as
// <Issuer>|<Subject>; Audience is one name of LUX_OIDC_AUDIENCE; the token
// is issued at IssuedAt and expires TTL later.
type Claims struct {
	Issuer   string
	Subject  string
	Audience string
	IssuedAt time.Time
	TTL      time.Duration
}

// header is the JOSE header of a minted token.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// payload is the claim set of a minted token. aud is one string, the
// form RFC 7519 allows for a single audience. There is no nbf: the
// verifier reads a local token on its own clock with no skew, and a
// token minted on a machine whose clock runs ahead would otherwise be
// refused until the two clocks agree, while iat and exp already bound
// the token.
type payload struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// Mint signs one compact JWS for the claims: the header names the key's
// algorithm and id, the payload carries iss, sub, aud, iat, exp, and a
// random jti, and the signature is the one the key's family writes. An
// empty issuer, subject, or audience, and a TTL that is not above zero,
// are refused, since each would be a token the verifier refuses.
func (k *Key) Mint(c Claims) (string, error) {
	switch {
	case c.Issuer == "" || c.Subject == "" || c.Audience == "":
		return "", errors.New("localissuer: a token names an issuer, a subject, and an audience")
	case c.TTL <= 0:
		return "", errors.New("localissuer: a token's lifetime is above zero, not " + c.TTL.String())
	}
	head, err := json.Marshal(header{Alg: k.alg, Typ: "JWT", Kid: k.id})
	if err != nil {
		return "", fmt.Errorf("localissuer: encoding the header: %w", err)
	}
	body, err := json.Marshal(payload{
		Iss: c.Issuer, Sub: c.Subject, Aud: c.Audience,
		Iat: c.IssuedAt.Unix(), Exp: c.IssuedAt.Add(c.TTL).Unix(),
		Jti: rand.Text(),
	})
	if err != nil {
		return "", fmt.Errorf("localissuer: encoding the claims: %w", err)
	}
	input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig, err := k.sign(input)
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// sign signs the JWS signing input. ES256 is the fixed-width r||s of RFC
// 7518 section 3.4, each half left-padded to 32 bytes, and not the ASN.1
// sequence crypto.Signer returns for an ECDSA key; RS256 is PKCS#1 v1.5
// over SHA-256.
func (k *Key) sign(input string) ([]byte, error) {
	digest := sha256.Sum256([]byte(input))
	if k.ec != nil {
		r, s, err := ecdsa.Sign(rand.Reader, k.ec, digest[:])
		if err != nil {
			return nil, fmt.Errorf("localissuer: signing with ES256 key %s: %w", k.id, err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return sig, nil
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, digest[:])
	if err != nil {
		return nil, fmt.Errorf("localissuer: signing with RS256 key %s: %w", k.id, err)
	}
	return sig, nil
}
