package license

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"sync"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// productionTrustManifest is the vendor trust manifest compiled into
// release binaries, in the format written by the issuer's export-trust
// command. It is filled in before release; until then no production
// certificate verifies.
const productionTrustManifest = ``

// ProductionTrustManifest returns the production trust manifest exactly as
// compiled in; the installation package build records it in package.json.
func ProductionTrustManifest() string { return productionTrustManifest }

// testTrustManifest adds non-production keys at build time:
//
//	-ldflags "-X github.com/seaweedfs/seaweedfs/weed/license.testTrustManifest=<base64url of manifest JSON>"
var testTrustManifest string

const (
	maxTrustKeys    = 32
	trustPurposeKey = "license"
)

var spkiEd25519Prefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// TrustStore holds the Ed25519 keys that may sign license certificates.
type TrustStore struct {
	keys map[string]ed25519.PublicKey
	test bool
}

// Lookup returns the license key for kid.
func (t *TrustStore) Lookup(kid string) (ed25519.PublicKey, bool) {
	key, ok := t.keys[kid]
	return key, ok
}

// IsTest reports whether keys outside the production manifest are trusted.
func (t *TrustStore) IsTest() bool {
	return t.test
}

var (
	defaultTrustOnce  sync.Once
	defaultTrust      *TrustStore
	defaultTrustError error
)

// DefaultTrustStore returns the compiled-in trust store, including any
// test manifest injected at build time.
func DefaultTrustStore() (*TrustStore, error) {
	defaultTrustOnce.Do(func() {
		var test []byte
		if testTrustManifest != "" {
			if test, defaultTrustError = decodeSegment(testTrustManifest); defaultTrustError != nil {
				defaultTrustError = fmt.Errorf("test trust manifest: %v", defaultTrustError)
				return
			}
			glog.Warningf("license: non-production trust root is compiled into this binary")
		}
		defaultTrust, defaultTrustError = NewTrustStore([]byte(productionTrustManifest), test)
	})
	return defaultTrust, defaultTrustError
}

// NewTrustStore builds a trust store from a production manifest and an
// optional test manifest; either may be empty. A kid may appear only once
// across both.
func NewTrustStore(production, test []byte) (*TrustStore, error) {
	t := &TrustStore{keys: make(map[string]ed25519.PublicKey), test: len(test) > 0}
	for _, manifest := range [][]byte{production, test} {
		if len(manifest) == 0 {
			continue
		}
		keys, err := parseTrustManifest(manifest)
		if err != nil {
			return nil, err
		}
		for kid, key := range keys {
			if _, dup := t.keys[kid]; dup {
				return nil, fmt.Errorf("trust manifest: duplicate kid %q", kid)
			}
			t.keys[kid] = key
		}
	}
	return t, nil
}

// parseTrustManifest validates a manifest exactly as the issuer does and
// returns its license-purpose keys; time_repair keys are not used by OSS.
func parseTrustManifest(data []byte) (map[string]ed25519.PublicKey, error) {
	invalid := fmt.Errorf("invalid trust manifest")
	obj, err := parseStrictJSON(data)
	if err != nil || !exactFields(obj, "schema_version", "keys") {
		return nil, invalid
	}
	if v, ok := obj["schema_version"].(int64); !ok || v != 1 {
		return nil, invalid
	}
	entries, ok := obj["keys"].([]interface{})
	if !ok || len(entries) < 1 || len(entries) > maxTrustKeys {
		return nil, invalid
	}
	keys := make(map[string]ed25519.PublicKey)
	seen := make(map[string]bool)
	purposeOf := make(map[string]string)
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok || !exactFields(entry, "kid", "purpose", "public_key_spki") {
			return nil, invalid
		}
		kid, _ := entry["kid"].(string)
		purpose, _ := entry["purpose"].(string)
		encoded, _ := entry["public_key_spki"].(string)
		if !validText(kid, 128, false) || seen[kid] || (purpose != trustPurposeKey && purpose != "time_repair") {
			return nil, invalid
		}
		seen[kid] = true
		spki, err := decodeSegment(encoded)
		if err != nil || len(spki) != len(spkiEd25519Prefix)+ed25519.PublicKeySize || !bytes.HasPrefix(spki, spkiEd25519Prefix) {
			return nil, invalid
		}
		material := string(spki)
		if previous, used := purposeOf[material]; used && previous != purpose {
			return nil, fmt.Errorf("trust manifest: one key used for both license and time_repair")
		}
		purposeOf[material] = purpose
		if purpose == trustPurposeKey {
			keys[kid] = ed25519.PublicKey(spki[len(spkiEd25519Prefix):])
		}
	}
	return keys, nil
}

func exactFields(obj map[string]interface{}, names ...string) bool {
	if len(obj) != len(names) {
		return false
	}
	for _, n := range names {
		if _, ok := obj[n]; !ok {
			return false
		}
	}
	return true
}
