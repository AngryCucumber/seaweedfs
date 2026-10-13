package license

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

const (
	// MaxCertificateBytes bounds a certificate's compact serialization.
	MaxCertificateBytes = 64 * 1024
	certificateType     = "massdb-license+jws"
)

// Certificate is a verified license certificate. It is immutable.
type Certificate struct {
	raw    string
	kid    string
	sha256 string
	Claims Claims
}

// Raw returns the exact compact JWS that was verified.
func (c *Certificate) Raw() string { return c.raw }

// Kid returns the trust key that signed the certificate.
func (c *Certificate) Kid() string { return c.kid }

// SHA256 returns the hex SHA-256 fingerprint of the raw certificate.
func (c *Certificate) SHA256() string { return c.sha256 }

// SameBytes reports whether two certificates are byte-identical.
func (c *Certificate) SameBytes(o *Certificate) bool {
	return c != nil && o != nil && c.raw == o.raw
}

// ParseCertificate verifies a compact JWS certificate against the trust
// store and validates its claims. Errors carry CodeMalformed,
// CodeUntrustedKid, CodeSignatureInvalid or CodeProductMismatch. The
// deployment, time and sequence checks belong to the caller.
func ParseCertificate(raw []byte, trust *TrustStore) (*Certificate, error) {
	if len(raw) > MaxCertificateBytes {
		return nil, errorf(CodeMalformed, "certificate exceeds %d bytes", MaxCertificateBytes)
	}
	parts := bytes.Split(raw, []byte{'.'})
	if len(parts) != 3 {
		return nil, errorf(CodeMalformed, "certificate must have three JWS segments")
	}
	headerBytes, err := decodeSegment(string(parts[0]))
	if err != nil {
		return nil, errorf(CodeMalformed, "header: %v", err)
	}
	header, err := parseStrictJSON(headerBytes)
	if err != nil || !exactFields(header, "alg", "typ", "kid") {
		return nil, errorf(CodeMalformed, "header must contain exactly alg, typ and kid")
	}
	if header["alg"] != "Ed25519" || header["typ"] != certificateType {
		return nil, errorf(CodeMalformed, "unsupported JWS algorithm or type")
	}
	kid, _ := header["kid"].(string)
	if !validText(kid, 128, false) {
		return nil, errorf(CodeMalformed, "invalid kid")
	}
	key, ok := trust.Lookup(kid)
	if !ok {
		return nil, errorf(CodeUntrustedKid, "kid %q is not trusted", kid)
	}
	signature, err := decodeSegment(string(parts[2]))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errorf(CodeMalformed, "signature must be 64 bytes of canonical base64url")
	}
	signingInput := raw[:len(parts[0])+1+len(parts[1])]
	if !ed25519.Verify(key, signingInput, signature) {
		return nil, errorf(CodeSignatureInvalid, "signature does not verify")
	}
	payload, err := decodeSegment(string(parts[1]))
	if err != nil {
		return nil, errorf(CodeMalformed, "payload: %v", err)
	}
	obj, err := parseStrictJSON(payload)
	if err != nil {
		return nil, errorf(CodeMalformed, "payload is not protocol v1 JSON")
	}
	claims, err := claimsFromJSON(obj)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &Certificate{raw: string(raw), kid: kid, sha256: hex.EncodeToString(sum[:]), Claims: *claims}, nil
}

var errSegment = errors.New("not canonical unpadded base64url")

// decodeSegment accepts only canonical, unpadded base64url.
func decodeSegment(s string) ([]byte, error) {
	if s == "" {
		return nil, errSegment
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, errSegment
		}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != s {
		return nil, errSegment
	}
	return decoded, nil
}
