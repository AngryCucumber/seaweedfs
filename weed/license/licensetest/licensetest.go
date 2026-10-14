// Package licensetest builds license nodes in a chosen state for the tests
// of packages that guard reads. Only tests import it.
package licensetest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/license"
)

const (
	// ClusterID is the deployment of the certificates built here.
	ClusterID = "0b6c6f58-3c39-4f6e-9a0e-6a3f0b9d4c21"
	kid       = "licensetest"
)

var (
	signingKey = ed25519.NewKeyFromSeed([]byte("massdb-oss-licensetest-signing-k"))
	// InternalKey is the internal_read_key the nodes built here receive.
	InternalKey = []byte("massdb-oss-licensetest-read-key!")
	// spkiPrefix is the DER prefix of an Ed25519 SubjectPublicKeyInfo.
	spkiPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}
)

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// Trust returns a trust store holding the test signing key only.
func Trust(t testing.TB) *license.TrustStore {
	t.Helper()
	spki := append(append([]byte(nil), spkiPrefix...), signingKey.Public().(ed25519.PublicKey)...)
	manifest, _ := json.Marshal(map[string]interface{}{
		"schema_version": 1,
		"keys":           []interface{}{map[string]interface{}{"kid": kid, "purpose": "license", "public_key_spki": b64(spki)}},
	})
	trust, err := license.NewTrustStore(nil, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

// Certificate signs a certificate of ClusterID valid in [notBefore,
// expiresAt) with the given features.
func Certificate(sequence, notBefore, expiresAt int64, features ...string) string {
	f := make([]interface{}, len(features))
	for i, s := range features {
		f[i] = s
	}
	claims := map[string]interface{}{
		"schema_version": 1, "policy_version": 1, "license_id": "licensetest",
		"issuer": "MassDB Issuer", "customer_id": "Test", "edition": "Enterprise",
		"product": license.Product, "deployment_id": ClusterID,
		"issued_at": notBefore, "not_before": notBefore, "expires_at": expiresAt,
		"sequence": sequence, "features": f,
		"limits": map[string]interface{}{"max_master_nodes": 3, "max_volume_servers": 10, "max_filers": 10},
	}
	header, _ := json.Marshal(map[string]string{"alg": "Ed25519", "typ": "massdb-license+jws", "kid": kid})
	payload, _ := json.Marshal(claims)
	input := b64(header) + "." + b64(payload)
	return input + "." + b64(ed25519.Sign(signingKey, []byte(input)))
}

// Node returns a node that received certificate (none if empty) and
// InternalKey from a master.
func Node(t testing.TB, certificate string) *license.Node {
	t.Helper()
	n := license.NewNode(Trust(t), "")
	n.Apply(&license.Update{
		NewestCertificate: certificate,
		MasterNowUnixMs:   time.Now().UnixMilli(),
		ClusterID:         ClusterID,
		InternalReadKey:   InternalKey,
	})
	return n
}

// ValidNode returns a node whose license allows reads.
func ValidNode(t testing.TB) *license.Node {
	now := time.Now().Unix()
	return Node(t, Certificate(1, now-3600, now+3600, license.FeatureDataRead))
}

// DeniedNode returns a node without a license (NO_LICENSE) that has the
// internal read key.
func DeniedNode(t testing.TB) *license.Node {
	return Node(t, "")
}

// NotReadyNode returns a node that never heard from a master.
func NotReadyNode(t testing.TB) *license.Node {
	return license.NewNode(Trust(t), "")
}

// Token returns the encoded InternalKey.
func Token() string {
	return license.EncodeInternalToken(InternalKey)
}
