package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

const (
	testDeployment = "c9b82051-a34e-455b-bff3-2f09306c6aa8"
	testKid        = "test-2026"
	// 2026-01-01T00:00:00Z and 2027-01-01T00:00:00Z.
	t2026 = 1767225600
	t2027 = 1798761600
)

var testKey = ed25519.NewKeyFromSeed([]byte("massdb-oss-license-unit-test-key"))

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func trustManifestFor(kid string, pub ed25519.PublicKey) []byte {
	spki := append(append([]byte(nil), spkiEd25519Prefix...), pub...)
	m, _ := json.Marshal(map[string]interface{}{
		"schema_version": 1,
		"keys":           []interface{}{map[string]interface{}{"kid": kid, "purpose": "license", "public_key_spki": b64(spki)}},
	})
	return m
}

func testTrust(t testing.TB) *TrustStore {
	t.Helper()
	trust, err := NewTrustStore(nil, trustManifestFor(testKid, testKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

// baseClaims returns valid claims; tests edit the map before signing.
func baseClaims() map[string]interface{} {
	return map[string]interface{}{
		"schema_version": 1, "policy_version": 1, "license_id": "oss-001",
		"issuer": "MassDB Issuer", "customer_id": "Customer A", "edition": "Enterprise",
		"product": Product, "deployment_id": testDeployment,
		"issued_at": t2026, "not_before": t2026, "expires_at": t2027,
		"sequence": 1, "features": []interface{}{FeatureDataRead},
		"limits": map[string]interface{}{"max_master_nodes": 3, "max_volume_servers": 10, "max_filers": 2},
	}
}

func signRaw(header, payload []byte) string {
	input := b64(header) + "." + b64(payload)
	return input + "." + b64(ed25519.Sign(testKey, []byte(input)))
}

func signClaims(claims map[string]interface{}) string {
	header, _ := json.Marshal(map[string]string{"alg": "Ed25519", "typ": certificateType, "kid": testKid})
	payload, _ := json.Marshal(claims)
	return signRaw(header, payload)
}

// mustCert signs claims edited by edit and parses the result.
func mustCert(t testing.TB, edit func(map[string]interface{})) *Certificate {
	t.Helper()
	claims := baseClaims()
	if edit != nil {
		edit(claims)
	}
	c, err := ParseCertificate([]byte(signClaims(claims)), testTrust(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func certWith(t testing.TB, seq, notBefore, expiresAt int64, features ...string) *Certificate {
	return mustCert(t, func(c map[string]interface{}) {
		c["sequence"], c["not_before"], c["expires_at"] = seq, notBefore, expiresAt
		c["issued_at"] = min(notBefore, int64(t2026))
		f := make([]interface{}, len(features))
		for i, s := range features {
			f[i] = s
		}
		c["features"] = f
	})
}
