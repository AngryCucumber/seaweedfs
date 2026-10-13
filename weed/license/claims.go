package license

import "regexp"

const (
	// Product is the only product accepted by MassDB OSS.
	Product = "MassDB OSS"
	// FeatureDataRead grants reading file content.
	FeatureDataRead = "DATA_READ"

	maxEpochSecond = 253402300799
	maxSequence    = 9223372036854775807
	maxNodeLimit   = 2147483647
	maxFeatures    = 128
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Limits are the node quotas of a certificate.
type Limits struct {
	MaxMasterNodes   int64 `json:"max_master_nodes"`
	MaxVolumeServers int64 `json:"max_volume_servers"`
	MaxFilers        int64 `json:"max_filers"`
}

// Min returns the smaller quota of each node type.
func (l Limits) Min(o Limits) Limits {
	return Limits{
		MaxMasterNodes:   min(l.MaxMasterNodes, o.MaxMasterNodes),
		MaxVolumeServers: min(l.MaxVolumeServers, o.MaxVolumeServers),
		MaxFilers:        min(l.MaxFilers, o.MaxFilers),
	}
}

// Claims are the protocol v1 certificate claims with the OSS limits.
type Claims struct {
	SchemaVersion int64    `json:"schema_version"`
	PolicyVersion int64    `json:"policy_version"`
	LicenseID     string   `json:"license_id"`
	Issuer        string   `json:"issuer"`
	CustomerID    string   `json:"customer_id"`
	Edition       string   `json:"edition"`
	Product       string   `json:"product"`
	DeploymentID  string   `json:"deployment_id"`
	IssuedAt      int64    `json:"issued_at"`
	NotBefore     int64    `json:"not_before"`
	ExpiresAt     int64    `json:"expires_at"`
	Sequence      int64    `json:"sequence"`
	Features      []string `json:"features"`
	Limits        Limits   `json:"limits"`
}

var claimNames = []string{
	"schema_version", "policy_version", "license_id", "issuer", "customer_id",
	"product", "deployment_id", "issued_at", "not_before", "expires_at",
	"sequence", "edition", "features", "limits",
}

// claimsFromJSON validates the claim object field by field. A wrong product
// is reported as CodeProductMismatch, every other problem as CodeMalformed.
func claimsFromJSON(obj map[string]interface{}) (*Claims, error) {
	malformed := func(field string) error { return errorf(CodeMalformed, "invalid claim %s", field) }
	if !exactFields(obj, claimNames...) {
		return nil, errorf(CodeMalformed, "claims must contain exactly the protocol v1 fields")
	}
	c := &Claims{}
	var ok bool
	integer := func(name string, lo, hi int64, dst *int64) bool {
		v, isInt := obj[name].(int64)
		if !isInt || v < lo || v > hi {
			return false
		}
		*dst = v
		return true
	}
	text := func(name string, allowSpaces bool, dst *string) bool {
		v, isStr := obj[name].(string)
		if !isStr || !validText(v, 256, allowSpaces) {
			return false
		}
		*dst = v
		return true
	}
	if !integer("schema_version", 1, 1, &c.SchemaVersion) {
		return nil, malformed("schema_version")
	}
	if !integer("policy_version", 1, 1, &c.PolicyVersion) {
		return nil, malformed("policy_version")
	}
	if c.Product, ok = obj["product"].(string); !ok {
		return nil, malformed("product")
	}
	if c.Product != Product {
		return nil, errorf(CodeProductMismatch, "certificate is not for %s", Product)
	}
	if !text("license_id", false, &c.LicenseID) {
		return nil, malformed("license_id")
	}
	for name, dst := range map[string]*string{"issuer": &c.Issuer, "customer_id": &c.CustomerID, "edition": &c.Edition} {
		if !text(name, true, dst) {
			return nil, malformed(name)
		}
	}
	if c.DeploymentID, ok = obj["deployment_id"].(string); !ok || !canonicalUUID.MatchString(c.DeploymentID) {
		return nil, malformed("deployment_id")
	}
	for name, dst := range map[string]*int64{"issued_at": &c.IssuedAt, "not_before": &c.NotBefore, "expires_at": &c.ExpiresAt} {
		if !integer(name, 0, maxEpochSecond, dst) {
			return nil, malformed(name)
		}
	}
	if c.IssuedAt > c.ExpiresAt || c.NotBefore >= c.ExpiresAt {
		return nil, malformed("time ordering")
	}
	if !integer("sequence", 1, maxSequence, &c.Sequence) {
		return nil, malformed("sequence")
	}
	features, ok := obj["features"].([]interface{})
	if !ok || len(features) > maxFeatures {
		return nil, malformed("features")
	}
	seen := make(map[string]bool, len(features))
	c.Features = make([]string, 0, len(features))
	for _, f := range features {
		name, isStr := f.(string)
		if !isStr || !validText(name, 128, false) || seen[name] {
			return nil, malformed("features")
		}
		seen[name] = true
		c.Features = append(c.Features, name)
	}
	limits, ok := obj["limits"].(map[string]interface{})
	if !ok || !exactFields(limits, "max_master_nodes", "max_volume_servers", "max_filers") {
		return nil, malformed("limits")
	}
	for name, dst := range map[string]*int64{
		"max_master_nodes": &c.Limits.MaxMasterNodes, "max_volume_servers": &c.Limits.MaxVolumeServers, "max_filers": &c.Limits.MaxFilers,
	} {
		v, isInt := limits[name].(int64)
		if !isInt || v < 1 || v > maxNodeLimit {
			return nil, malformed("limits." + name)
		}
		*dst = v
	}
	return c, nil
}

// HasFeature reports whether the claims grant a feature.
func (c *Claims) HasFeature(feature string) bool {
	for _, f := range c.Features {
		if f == feature {
			return true
		}
	}
	return false
}
