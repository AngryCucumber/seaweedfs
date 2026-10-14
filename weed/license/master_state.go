package license

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	// StateDirName is the license directory under the master -mdir.
	StateDirName = "license"
	// StateFileName holds a master's license state, clock and ledger.
	StateFileName     = "state.json"
	stateSchema       = 1
	maxStateFileBytes = 64 << 20
)

// State is the license state replicated between masters
// (master_pb.LicenseState). Certificates are raw compact JWS.
type State struct {
	ClusterID       string
	Newest          string
	Prev            string
	InternalReadKey []byte
	Clock           ClockState
}

// LedgerSnapshot is a versioned copy of the member ledger
// (master_pb.NodeLedgerSnapshot).
type LedgerSnapshot struct {
	LedgerVersion
	Entries []LedgerEntry `json:"entries"`
}

// stateFile is <mdir>/license/state.json.
type stateFile struct {
	SchemaVersion   int             `json:"schema_version"`
	ClusterID       string          `json:"cluster_id"`
	Newest          string          `json:"newest,omitempty"`
	Prev            string          `json:"prev,omitempty"`
	InternalReadKey string          `json:"internal_read_key,omitempty"`
	Watermark       int64           `json:"watermark"`
	RepairSeq       int64           `json:"repair_seq"`
	Ledger          *LedgerSnapshot `json:"ledger,omitempty"`
}

func readStateFile(path string) (*stateFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxStateFileBytes)
	}
	var s stateFile
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if s.SchemaVersion != stateSchema {
		return nil, fmt.Errorf("%s: unsupported schema_version %d", path, s.SchemaVersion)
	}
	if s.ClusterID != "" && !canonicalUUID.MatchString(s.ClusterID) {
		return nil, fmt.Errorf("%s: cluster_id %q is not a canonical lowercase UUID", path, s.ClusterID)
	}
	return &s, nil
}

func writeStateFile(path string, s *stateFile) error {
	s.SchemaVersion = stateSchema
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func decodeInternalKey(s string) []byte {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != InternalKeySize {
		return nil
	}
	return key
}

func newInternalKey() []byte {
	key := make([]byte, InternalKeySize)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

func newClusterID() string {
	return uuid.New().String()
}

// parseCertPair verifies a (newest, prev) pair. An empty newest means no
// certificate; prev without newest is malformed.
func parseCertPair(trust *TrustStore, newest, prev string) (Certs, error) {
	var c Certs
	if newest == "" {
		if prev != "" {
			return c, errorf(CodeMalformed, "previous certificate without a newest one")
		}
		return c, nil
	}
	var err error
	if c.Newest, err = ParseCertificate([]byte(newest), trust); err != nil {
		return Certs{}, err
	}
	if prev != "" {
		if c.Prev, err = ParseCertificate([]byte(prev), trust); err != nil {
			return Certs{}, err
		}
	}
	return c, nil
}

func rawOf(c *Certificate) string {
	if c == nil {
		return ""
	}
	return c.Raw()
}

// DeploymentRequest is the license request DTO (GET /license/deployment,
// contract 1.4). Field order is part of the contract.
type DeploymentRequest struct {
	SchemaVersion           int    `json:"schema_version"`
	Product                 string `json:"product"`
	DeploymentID            string `json:"deployment_id"`
	RegisteredMasterNodes   int64  `json:"registered_master_nodes"`
	RegisteredVolumeServers int64  `json:"registered_volume_servers"`
	RegisteredFilers        int64  `json:"registered_filers"`
}

func newDeploymentRequest(clusterID string, used Usage) DeploymentRequest {
	return DeploymentRequest{
		SchemaVersion:           1,
		Product:                 Product,
		DeploymentID:            clusterID,
		RegisteredMasterNodes:   used.Masters,
		RegisteredVolumeServers: used.VolumeServers,
		RegisteredFilers:        used.Filers,
	}
}

// InitState is the offline `weed license.init`: it makes sure
// <mdir>/license/state.json has a cluster_id, generating one unless
// clusterID is given, and returns the request DTO with zero registrations.
// An existing different cluster_id is never overwritten.
func InitState(mdir, clusterID string) (DeploymentRequest, error) {
	if clusterID != "" && !canonicalUUID.MatchString(clusterID) {
		return DeploymentRequest{}, fmt.Errorf("-clusterId %q is not a canonical lowercase UUID", clusterID)
	}
	path := filepath.Join(mdir, StateDirName, StateFileName)
	s, err := readStateFile(path)
	switch {
	case os.IsNotExist(err):
		s = &stateFile{}
	case err != nil:
		return DeploymentRequest{}, err
	}
	switch {
	case s.ClusterID != "" && clusterID != "" && s.ClusterID != clusterID:
		return DeploymentRequest{}, fmt.Errorf("%s already has cluster_id %s; refusing to replace it with %s", path, s.ClusterID, clusterID)
	case s.ClusterID == "":
		s.ClusterID = clusterID
		if s.ClusterID == "" {
			s.ClusterID = newClusterID()
		}
		if err := writeStateFile(path, s); err != nil {
			return DeploymentRequest{}, err
		}
	}
	return newDeploymentRequest(s.ClusterID, Usage{}), nil
}

// CertificateSummary describes a certificate without its signed text.
type CertificateSummary struct {
	LicenseID  string   `json:"license_id"`
	Issuer     string   `json:"issuer"`
	CustomerID string   `json:"customer_id"`
	Edition    string   `json:"edition"`
	Sequence   int64    `json:"sequence"`
	IssuedAt   int64    `json:"issued_at"`
	NotBefore  int64    `json:"not_before"`
	ExpiresAt  int64    `json:"expires_at"`
	Features   []string `json:"features"`
	Limits     Limits   `json:"limits"`
	Kid        string   `json:"kid"`
	SHA256     string   `json:"sha256"`
}

// Summary returns the certificate summary, or nil for nil.
func (c *Certificate) Summary() *CertificateSummary {
	if c == nil {
		return nil
	}
	return &CertificateSummary{
		LicenseID:  c.Claims.LicenseID,
		Issuer:     c.Claims.Issuer,
		CustomerID: c.Claims.CustomerID,
		Edition:    c.Claims.Edition,
		Sequence:   c.Claims.Sequence,
		IssuedAt:   c.Claims.IssuedAt,
		NotBefore:  c.Claims.NotBefore,
		ExpiresAt:  c.Claims.ExpiresAt,
		Features:   append([]string{}, c.Claims.Features...),
		Limits:     c.Claims.Limits,
		Kid:        c.Kid(),
		SHA256:     c.SHA256(),
	}
}
