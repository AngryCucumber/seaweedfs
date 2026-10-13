package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestIssuerInteropVectors(t *testing.T) {
	manifest, err := os.ReadFile("testdata/issuer_trust.json")
	if err != nil {
		t.Fatal(err)
	}
	trust, err := NewTrustStore(manifest, nil)
	if err != nil {
		t.Fatalf("issuer trust manifest: %v", err)
	}
	raw, _ := os.ReadFile("testdata/issuer_valid.jws")
	c, err := ParseCertificate(raw, trust)
	if err != nil {
		t.Fatalf("issuer-signed certificate: %v", err)
	}
	var meta struct {
		Kid    string `json:"kid"`
		Claims Claims `json:"claims"`
	}
	data, _ := os.ReadFile("testdata/issuer_valid.claims.json")
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if c.Kid() != meta.Kid || c.Claims.CustomerID != meta.Claims.CustomerID || c.Claims.Limits != meta.Claims.Limits ||
		c.Claims.ExpiresAt != meta.Claims.ExpiresAt || !c.Claims.HasFeature(FeatureDataRead) {
		t.Fatalf("claims differ from the issuer input: %+v", c.Claims)
	}
	if c.Raw() != string(raw) || len(c.SHA256()) != 64 {
		t.Fatal("raw or fingerprint not kept")
	}

	sql, _ := os.ReadFile("testdata/issuer_sql_product.jws")
	if _, err := ParseCertificate(sql, trust); codeOf(err) != CodeProductMismatch {
		t.Fatalf("MassDB SQL certificate: got %v, want %s", err, CodeProductMismatch)
	}

	tampered := []byte(strings.Replace(string(raw), ".", ".e", 1))
	if _, err := ParseCertificate(tampered, trust); err == nil {
		t.Fatal("tampered certificate accepted")
	}
}

func TestParseCertificateHeaderAndEncoding(t *testing.T) {
	trust := testTrust(t)
	valid := signClaims(baseClaims())
	payload, _ := json.Marshal(baseClaims())
	header := func(fields map[string]interface{}) []byte {
		h, _ := json.Marshal(fields)
		return h
	}
	parts := strings.Split(valid, ".")
	cases := map[string]struct {
		raw  string
		code Code
	}{
		"two segments":    {parts[0] + "." + parts[1], CodeMalformed},
		"four segments":   {valid + ".x", CodeMalformed},
		"padded header":   {parts[0] + "=." + parts[1] + "." + parts[2], CodeMalformed},
		"std alphabet":    {"+" + parts[0][1:] + "." + parts[1] + "." + parts[2], CodeMalformed},
		"empty payload":   {parts[0] + ".." + parts[2], CodeSignatureInvalid},
		"alg none":        {signRaw(header(map[string]interface{}{"alg": "none", "typ": certificateType, "kid": testKid}), payload), CodeMalformed},
		"alg EdDSA":       {signRaw(header(map[string]interface{}{"alg": "EdDSA", "typ": certificateType, "kid": testKid}), payload), CodeMalformed},
		"wrong typ":       {signRaw(header(map[string]interface{}{"alg": "Ed25519", "typ": "JWT", "kid": testKid}), payload), CodeMalformed},
		"extra header":    {signRaw(header(map[string]interface{}{"alg": "Ed25519", "typ": certificateType, "kid": testKid, "crit": []string{"b64"}}), payload), CodeMalformed},
		"missing kid":     {signRaw(header(map[string]interface{}{"alg": "Ed25519", "typ": certificateType}), payload), CodeMalformed},
		"unknown kid":     {signRaw(header(map[string]interface{}{"alg": "Ed25519", "typ": certificateType, "kid": "other"}), payload), CodeUntrustedKid},
		"short signature": {parts[0] + "." + parts[1] + "." + parts[2][:40], CodeMalformed},
		"bad signature":   {parts[0] + "." + parts[1] + "." + b64(make([]byte, 64)), CodeSignatureInvalid},
		"other payload":   {parts[0] + "." + b64([]byte(`{}`)) + "." + parts[2], CodeSignatureInvalid},
		"too large":       {valid + strings.Repeat("A", MaxCertificateBytes), CodeMalformed},
	}
	for name, c := range cases {
		if _, err := ParseCertificate([]byte(c.raw), trust); codeOf(err) != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}

	// A non-canonical last character (non-zero trailing bits) must not verify
	// as the same signature segment.
	sig := parts[2]
	last := sig[len(sig)-1]
	alt := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", last) ^ 1
	altSig := sig[:len(sig)-1] + string("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[alt])
	if _, err := ParseCertificate([]byte(parts[0]+"."+parts[1]+"."+altSig), trust); err == nil {
		t.Error("non-canonical signature encoding accepted")
	}
}

func TestClaimsValidation(t *testing.T) {
	trust := testTrust(t)
	cases := map[string]struct {
		edit func(map[string]interface{})
		code Code
	}{
		"extra claim":        {func(c map[string]interface{}) { c["extra"] = 1 }, CodeMalformed},
		"missing claim":      {func(c map[string]interface{}) { delete(c, "edition") }, CodeMalformed},
		"schema 2":           {func(c map[string]interface{}) { c["schema_version"] = 2 }, CodeMalformed},
		"policy 2":           {func(c map[string]interface{}) { c["policy_version"] = 2 }, CodeMalformed},
		"sql product":        {func(c map[string]interface{}) { c["product"] = "MassDB SQL" }, CodeProductMismatch},
		"product not string": {func(c map[string]interface{}) { c["product"] = 1 }, CodeMalformed},
		"bool as integer":    {func(c map[string]interface{}) { c["sequence"] = true }, CodeMalformed},
		"string as integer":  {func(c map[string]interface{}) { c["sequence"] = "1" }, CodeMalformed},
		"sequence 0":         {func(c map[string]interface{}) { c["sequence"] = 0 }, CodeMalformed},
		"license_id space":   {func(c map[string]interface{}) { c["license_id"] = "oss 001" }, CodeMalformed},
		"customer edge":      {func(c map[string]interface{}) { c["customer_id"] = "A " }, CodeMalformed},
		"upper uuid":         {func(c map[string]interface{}) { c["deployment_id"] = strings.ToUpper(testDeployment) }, CodeMalformed},
		"braced uuid":        {func(c map[string]interface{}) { c["deployment_id"] = "{" + testDeployment + "}" }, CodeMalformed},
		"negative time":      {func(c map[string]interface{}) { c["issued_at"] = -1 }, CodeMalformed},
		"time beyond 9999":   {func(c map[string]interface{}) { c["expires_at"] = int64(maxEpochSecond) + 1 }, CodeMalformed},
		"not_before==expiry": {func(c map[string]interface{}) { c["not_before"] = t2027 }, CodeMalformed},
		"issued after expiry": {func(c map[string]interface{}) {
			c["issued_at"] = t2027 + 1
		}, CodeMalformed},
		"duplicate feature": {func(c map[string]interface{}) { c["features"] = []interface{}{"A", "A"} }, CodeMalformed},
		"feature space":     {func(c map[string]interface{}) { c["features"] = []interface{}{"DATA READ"} }, CodeMalformed},
		"features object":   {func(c map[string]interface{}) { c["features"] = map[string]interface{}{} }, CodeMalformed},
		"sql limits": {func(c map[string]interface{}) {
			c["limits"] = map[string]interface{}{"max_fe_nodes": 3, "max_be_nodes": 3}
		}, CodeMalformed},
		"limit 0": {func(c map[string]interface{}) {
			c["limits"] = map[string]interface{}{"max_master_nodes": 0, "max_volume_servers": 1, "max_filers": 1}
		}, CodeMalformed},
		"limit too big": {func(c map[string]interface{}) {
			c["limits"] = map[string]interface{}{"max_master_nodes": 1, "max_volume_servers": int64(maxNodeLimit) + 1, "max_filers": 1}
		}, CodeMalformed},
	}
	for name, c := range cases {
		claims := baseClaims()
		c.edit(claims)
		if _, err := ParseCertificate([]byte(signClaims(claims)), trust); codeOf(err) != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}

	ok := map[string]func(map[string]interface{}){
		"empty features":   func(c map[string]interface{}) { c["features"] = []interface{}{} },
		"unknown feature":  func(c map[string]interface{}) { c["features"] = []interface{}{"FUTURE", FeatureDataRead} },
		"issued==expiry-1": func(c map[string]interface{}) { c["issued_at"] = t2027 },
		"max values": func(c map[string]interface{}) {
			c["sequence"], c["expires_at"] = int64(maxSequence), int64(maxEpochSecond)
		},
		"chinese customer":  func(c map[string]interface{}) { c["customer_id"] = "客户 甲" },
		"future not_before": func(c map[string]interface{}) { c["not_before"] = t2027 - 1 },
		"max limits and one": func(c map[string]interface{}) {
			c["limits"] = map[string]interface{}{"max_master_nodes": 1, "max_volume_servers": maxNodeLimit, "max_filers": 1}
		},
	}
	for name, edit := range ok {
		claims := baseClaims()
		edit(claims)
		if _, err := ParseCertificate([]byte(signClaims(claims)), trust); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTrustManifest(t *testing.T) {
	pub := testKey.Public().(ed25519.PublicKey)
	spki := b64(append(append([]byte(nil), spkiEd25519Prefix...), pub...))
	other := b64(append(append([]byte(nil), spkiEd25519Prefix...), ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)...))
	entry := func(kid, purpose, key string) string {
		return `{"kid":"` + kid + `","purpose":"` + purpose + `","public_key_spki":"` + key + `"}`
	}
	manifest := func(entries ...string) []byte {
		return []byte(`{"schema_version":1,"keys":[` + strings.Join(entries, ",") + `]}`)
	}

	store, err := NewTrustStore(manifest(entry("a", "license", spki), entry("r", "time_repair", other)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Lookup("a"); !ok {
		t.Error("license key missing")
	}
	if _, ok := store.Lookup("r"); ok {
		t.Error("time_repair key must not verify licenses")
	}
	if store.IsTest() {
		t.Error("production-only store marked as test")
	}

	bad := map[string][]byte{
		"no keys":          manifest(),
		"schema 2":         []byte(`{"schema_version":2,"keys":[` + entry("a", "license", spki) + `]}`),
		"extra field":      []byte(`{"schema_version":1,"keys":[` + entry("a", "license", spki) + `],"x":1}`),
		"duplicate kid":    manifest(entry("a", "license", spki), entry("a", "license", other)),
		"unknown purpose":  manifest(entry("a", "signing", spki)),
		"key two purposes": manifest(entry("a", "license", spki), entry("b", "time_repair", spki)),
		"padded spki":      manifest(entry("a", "license", spki+"=")),
		"short spki":       manifest(entry("a", "license", spki[:40])),
		"rsa prefix":       manifest(entry("a", "license", b64(append([]byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x71, 0x03, 0x21, 0x00}, pub...)))),
		"kid with space":   manifest(entry("a b", "license", spki)),
		"too many keys":    manifest(strings.Repeat(entry("x", "license", spki)+",", 32) + entry("y", "license", spki)),
	}
	for name, m := range bad {
		if _, err := NewTrustStore(m, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Production and test manifests merge; a kid may not appear in both.
	test := manifest(entry("t", "license", other))
	merged, err := NewTrustStore(manifest(entry("a", "license", spki)), test)
	if err != nil || !merged.IsTest() {
		t.Fatalf("merge: %v", err)
	}
	if _, ok := merged.Lookup("t"); !ok {
		t.Error("test key missing after merge")
	}
	if _, err := NewTrustStore(manifest(entry("a", "license", spki)), manifest(entry("a", "license", other))); err == nil {
		t.Error("kid in both manifests accepted")
	}
}

// DefaultTrustStore loads once per process; no other test may call it.
func TestDefaultTrustStoreTestManifest(t *testing.T) {
	saved := testTrustManifest
	defer func() { testTrustManifest = saved }()
	testTrustManifest = base64.RawURLEncoding.EncodeToString(trustManifestFor(testKid, testKey.Public().(ed25519.PublicKey)))
	store, err := DefaultTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Lookup(testKid); !ok || !store.IsTest() {
		t.Fatal("injected test manifest not loaded")
	}
}
