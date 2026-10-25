package license

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
)

const (
	// InternalReadHeader carries the internal read token on filer
	// maintenance reads from volume servers.
	InternalReadHeader = "X-MassDB-Internal-Read"
	// InternalReadMetadataKey carries the same token on volume-to-volume
	// and filer-to-filer gRPC calls.
	InternalReadMetadataKey = "x-massdb-internal-read"
	// ReasonHeader tells clients, and the S3 gateway, why a read was denied.
	ReasonHeader = "X-MassDB-License-Reason"
	// ContentHiddenHeader marks metadata responses whose inline content was hidden.
	ContentHiddenHeader = "X-MassDB-Content-Hidden"
	// InternalKeySize is the size of internal_read_key.
	InternalKeySize = 32
)

// Snapshot is an immutable read decision. A guard swaps whole snapshots,
// so readers never lock.
type Snapshot struct {
	allowRead bool
	// reason is a deny reason that does not depend on time; empty when the
	// effective certificate decides.
	reason    Reason
	notBefore int64
	expiresAt int64
	clusterID string
	cert      *Certificate
	bodies    map[Reason][]byte
}

func newSnapshot(reason Reason, cert *Certificate, clusterID string) *Snapshot {
	s := &Snapshot{reason: reason, cert: cert, clusterID: clusterID}
	if cert != nil {
		s.notBefore, s.expiresAt = cert.Claims.NotBefore, cert.Claims.ExpiresAt
	}
	reasons := []Reason{reason}
	if reason == "" && cert != nil {
		s.allowRead = cert.Claims.HasFeature(FeatureDataRead)
		reasons = []Reason{ReasonNotYetValid, ReasonExpired, ReasonFeatureMissing}
	}
	s.bodies = make(map[Reason][]byte, len(reasons))
	for _, r := range reasons {
		s.bodies[r] = denyBody(r, s.expiresAt, clusterID)
	}
	return s
}

func denyBody(reason Reason, expiresAt int64, clusterID string) []byte {
	body, _ := json.Marshal(struct {
		Error     string `json:"error"`
		Reason    Reason `json:"reason"`
		ExpiresAt int64  `json:"expires_at"`
		ClusterID string `json:"cluster_id"`
	}{"license_denied", reason, expiresAt, clusterID})
	return body
}

// Reason returns why a read at now is denied. Call it only after AllowRead
// returned false.
func (s *Snapshot) Reason(now int64) Reason {
	switch {
	case s.reason != "":
		return s.reason
	case now < s.notBefore:
		return ReasonNotYetValid
	case now >= s.expiresAt:
		return ReasonExpired
	default:
		return ReasonFeatureMissing
	}
}

// Certificate returns the certificate in effect when the snapshot was built.
func (s *Snapshot) Certificate() *Certificate { return s.cert }

// Guard answers whether reads are allowed. AllowRead is lock-free and
// allocation-free; everything else runs only on the deny path.
type Guard struct {
	snap  atomic.Pointer[Snapshot]
	clock atomic.Int64
	key   atomic.Pointer[[InternalKeySize]byte]
}

func newGuard() *Guard {
	g := &Guard{}
	g.snap.Store(newSnapshot(ReasonNotReady, nil, ""))
	return g
}

// AllowRead reports whether a data read is allowed now. On false it also
// returns the snapshot to build the denial from.
func (g *Guard) AllowRead() (bool, *Snapshot) {
	s := g.snap.Load()
	now := g.clock.Load()
	if s.allowRead && now >= s.notBefore && now < s.expiresAt {
		return true, nil
	}
	return false, s
}

// Now returns the guard's clock in Unix seconds.
func (g *Guard) Now() int64 {
	return g.clock.Load()
}

// InternalTokenValid checks an internal read token from a request header or
// gRPC metadata. It never accepts while the node is NOT_READY or has no key.
func (g *Guard) InternalTokenValid(token string) bool {
	key := g.key.Load()
	if key == nil || g.snap.Load().reason == ReasonNotReady || len(token) != base64.StdEncoding.EncodedLen(InternalKeySize) {
		return false
	}
	var decoded [InternalKeySize + 1]byte
	n, err := base64.StdEncoding.Decode(decoded[:], []byte(token))
	if err != nil || n != InternalKeySize {
		return false
	}
	return subtle.ConstantTimeCompare(decoded[:n], key[:]) == 1
}

// WriteDenied writes the 403 license denial for snapshot s.
func (g *Guard) WriteDenied(w http.ResponseWriter, s *Snapshot) {
	reason := s.Reason(g.Now())
	body := s.bodies[reason]
	if body == nil {
		body = denyBody(reason, s.expiresAt, s.clusterID)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set(ReasonHeader, string(reason))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}

// WriteDeniedStatus writes a 403 with only the reason header, for entry
// points that run before the server's own authentication: the filer's
// chunk proxy (F2) sits before its JWT check, so an unauthenticated caller
// learns the reason but not cluster_id or expires_at.
func (g *Guard) WriteDeniedStatus(w http.ResponseWriter, s *Snapshot) {
	w.Header().Set(ReasonHeader, string(s.Reason(g.Now())))
	w.WriteHeader(http.StatusForbidden)
}

func (g *Guard) setKey(key []byte) {
	if len(key) != InternalKeySize {
		g.key.Store(nil)
		return
	}
	var k [InternalKeySize]byte
	copy(k[:], key)
	g.key.Store(&k)
}

// EncodeInternalToken encodes internal_read_key for InternalReadHeader and
// InternalReadMetadataKey.
func EncodeInternalToken(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}
