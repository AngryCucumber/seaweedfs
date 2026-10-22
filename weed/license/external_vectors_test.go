package license

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// externalVectorsEnv names a directory of certificates signed by another
// issuer implementation (the MassDB license center); the test is skipped when
// it is unset. The directory holds:
//
//	trust.json    the trust manifest exported by the issuer
//	trust.b64     optional: the same manifest as the base64url string given to
//	              -ldflags "-X .../weed/license.testTrustManifest=..."
//	vectors.json  {"vectors": [{"file": "valid.jws", "expect": "VALID",
//	              "kid": "...", "claims": {...}}, ...]}
//
// Each vector names a certificate file in the directory. expect is "VALID" or
// the reason code ParseCertificate must return (LICENSE_PRODUCT_MISMATCH,
// LICENSE_SIGNATURE_INVALID, ...). For a valid vector, kid and claims are
// optional and must match exactly when given. Certificate files are trimmed
// like -license.file and the import API do. Deployment, time and sequence
// checks are not part of parsing and cannot be expressed here.
const externalVectorsEnv = "SEAWEEDFS_LICENSE_VECTORS_DIR"

type externalVector struct {
	File   string          `json:"file"`
	Expect string          `json:"expect"`
	Kid    string          `json:"kid"`
	Claims json.RawMessage `json:"claims"`
}

func TestExternalIssuerVectors(t *testing.T) {
	dir := os.Getenv(externalVectorsEnv)
	if dir == "" {
		t.Skip("set " + externalVectorsEnv + " to a directory of externally signed vectors")
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	production, err := NewTrustStore(manifest, nil)
	if err != nil {
		t.Fatalf("trust.json as a production manifest: %v", err)
	}
	stores := map[string]*TrustStore{"trust.json": production}
	if encoded, err := os.ReadFile(filepath.Join(dir, "trust.b64")); err == nil {
		decoded, err := decodeSegment(string(bytes.TrimSpace(encoded)))
		if err != nil {
			t.Fatalf("trust.b64 is not canonical base64url: %v", err)
		}
		if !bytes.Equal(decoded, manifest) {
			t.Fatal("trust.b64 does not encode trust.json byte for byte")
		}
		test, err := NewTrustStore(nil, decoded)
		if err != nil || !test.IsTest() {
			t.Fatalf("trust.b64 as a test manifest: %v", err)
		}
		stores["trust.b64"] = test
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Vectors []externalVector `json:"vectors"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		t.Fatalf("vectors.json: %v", err)
	}
	if len(list.Vectors) == 0 {
		t.Fatal("vectors.json lists no vectors")
	}
	valid := 0
	for _, v := range list.Vectors {
		if v.File == "" || filepath.IsAbs(v.File) || v.File != filepath.Base(v.File) || v.Expect == "" {
			t.Fatalf("vector %+v: file must be a name in the directory and expect is required", v)
		}
		raw, err := os.ReadFile(filepath.Join(dir, v.File))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		if v.Expect == "VALID" {
			valid++
		}
		for source, trust := range stores {
			c, err := ParseCertificate(raw, trust)
			if v.Expect != "VALID" {
				if got := codeOf(err); string(got) != v.Expect {
					t.Errorf("%s with %s: got %v, want %s", v.File, source, err, v.Expect)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s with %s: %v", v.File, source, err)
				continue
			}
			sum := sha256.Sum256(raw)
			if c.Raw() != string(raw) || c.SHA256() != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: raw bytes or fingerprint not kept", v.File)
			}
			if v.Kid != "" && c.Kid() != v.Kid {
				t.Errorf("%s: kid %q, want %q", v.File, c.Kid(), v.Kid)
			}
			if len(v.Claims) > 0 {
				var want Claims
				claims := json.NewDecoder(bytes.NewReader(v.Claims))
				claims.DisallowUnknownFields()
				if err := claims.Decode(&want); err != nil {
					t.Fatalf("%s: expected claims: %v", v.File, err)
				}
				if !reflect.DeepEqual(c.Claims, want) {
					t.Errorf("%s: claims %+v, want %+v", v.File, c.Claims, want)
				}
			}
		}
	}
	if valid == 0 {
		t.Error("vectors.json has no VALID vector")
	}
	t.Logf("%d vectors from %s checked with %s", len(list.Vectors), dir, strings.Join(sortedKeys(stores), " and "))
}

func sortedKeys(m map[string]*TrustStore) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
