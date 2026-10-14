package license

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTime struct{ t time.Time }

func (f *fakeTime) now() time.Time        { return f.t }
func (f *fakeTime) add(d time.Duration)   { f.t = f.t.Add(d) }
func (f *fakeTime) set(unix int64)        { f.t = time.Unix(unix, 0) }
func newFakeTime(unix int64) *fakeTime    { return &fakeTime{t: time.Unix(unix, 0)} }
func longLived(t testing.TB) *Certificate { return certWith(t, 1, 0, maxEpochSecond, FeatureDataRead) }
func allow(n *Node) bool                  { ok, _ := n.Guard().AllowRead(); return ok }
func denyReason(n *Node) Reason {
	ok, s := n.Guard().AllowRead()
	if ok {
		return ""
	}
	return s.Reason(n.Guard().Now())
}

func newTestNode(t *testing.T, cachePath string, unix int64) (*Node, *fakeTime) {
	n := NewNode(testTrust(t), cachePath)
	ft := newFakeTime(unix)
	n.clock.mu.Lock()
	n.clock.timeNow = ft.now
	n.clock.mu.Unlock()
	return n, ft
}

func TestNodeApply(t *testing.T) {
	now := int64(t2026 + 100)
	n, _ := newTestNode(t, "", now)
	if denyReason(n) != ReasonNotReady {
		t.Fatalf("before the first frame: %s", denyReason(n))
	}
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000})
	if denyReason(n) != ReasonNoLicense {
		t.Fatalf("master without license: %s", denyReason(n))
	}

	key := bytes.Repeat([]byte{9}, InternalKeySize)
	c1 := certWith(t, 1, t2026, t2027, FeatureDataRead)
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000, NewestCertificate: c1.Raw(), InternalReadKey: key})
	if !allow(n) || !bytes.Equal(n.InternalKey(), key) || !n.Guard().InternalTokenValid(EncodeInternalToken(key)) {
		t.Fatal("valid certificate and key not applied")
	}

	keep := func(name string, u *Update) {
		t.Helper()
		u.ClusterID, u.MasterNowUnixMs, u.InternalReadKey = testDeployment, now*1000, key
		n.Apply(u)
		if !allow(n) || n.certs.Newest != c1 && !n.certs.Newest.SameBytes(c1) {
			t.Errorf("%s replaced the certificate", name)
		}
	}
	c0 := certWith(t, 0+1, t2026, t2027-5, FeatureDataRead) // same sequence, other bytes
	keep("same sequence other bytes", &Update{NewestCertificate: c0.Raw()})
	tampered := strings.Replace(c1.Raw(), ".", ".e", 1)
	keep("tampered", &Update{NewestCertificate: tampered})
	keep("empty", &Update{})

	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000, MasterDenyReason: string(ReasonClockSuspect)})
	if denyReason(n) != ReasonClockSuspect {
		t.Fatalf("master deny: %s", denyReason(n))
	}
	if n.InternalKey() != nil {
		t.Fatal("an update without key must clear the key")
	}
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000})
	if !allow(n) {
		t.Fatal("deny reason must clear on the next update")
	}

	n.Reject()
	if denyReason(n) != ReasonNodeRejected {
		t.Fatalf("rejected: %s", denyReason(n))
	}
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000})
	if !allow(n) {
		t.Fatal("an accepted update must clear NODE_REJECTED")
	}
}

func TestNodeRejectedBeforeFirstFrame(t *testing.T) {
	n, _ := newTestNode(t, "", t2026)
	n.Reject()
	if denyReason(n) != ReasonNodeRejected {
		t.Fatalf("a node refused at its first heartbeat reports NODE_REJECTED, got %s", denyReason(n))
	}
}

func TestNodeClockFollowsMaster(t *testing.T) {
	n, ft := newTestNode(t, "", 0) // local wall clock far off
	expiry := int64(t2026 + 200)
	c := certWith(t, 1, t2026, expiry, FeatureDataRead)
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: (t2026 + 100) * 1000, NewestCertificate: c.Raw()})
	if !allow(n) {
		t.Fatal("not anchored to the master clock")
	}
	ft.add(99 * time.Second)
	n.Tick()
	if !allow(n) {
		t.Fatal("expired one second early")
	}
	ft.add(time.Second)
	n.Tick()
	if denyReason(n) != ReasonExpired {
		t.Fatalf("at expiry: %s", denyReason(n))
	}
	// Changing the local wall clock does not matter once anchored.
	ft.set(0)
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: (t2026 + 150) * 1000})
	if !allow(n) {
		t.Fatal("re-anchor to master time")
	}
}

func TestNodePendingCertificateStarts(t *testing.T) {
	start := int64(t2026 + 1000)
	prev := certWith(t, 1, t2026, t2027) // no DATA_READ
	newest := certWith(t, 2, start, t2027, FeatureDataRead)
	n, ft := newTestNode(t, "", start-10)
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: (start - 10) * 1000, NewestCertificate: newest.Raw(), PrevCertificate: prev.Raw()})
	if denyReason(n) != ReasonFeatureMissing {
		t.Fatalf("prev is in effect: %s", denyReason(n))
	}
	ft.add(10 * time.Second)
	n.Tick()
	if !allow(n) {
		t.Fatalf("newest must take effect at not_before without a master update: %s", denyReason(n))
	}

	m, _ := newTestNode(t, "", start-10)
	m.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: (start - 10) * 1000, NewestCertificate: newest.Raw()})
	if denyReason(m) != ReasonNotYetValid {
		t.Fatalf("pending without prev: %s", denyReason(m))
	}
}

func TestNodeClusterBinding(t *testing.T) {
	now := int64(t2026 + 100)
	other := "00000000-0000-4000-8000-000000000000"
	n, _ := newTestNode(t, "", now)
	c := certWith(t, 1, t2026, t2027, FeatureDataRead)
	n.Apply(&Update{ClusterID: other, MasterNowUnixMs: now * 1000, NewestCertificate: c.Raw()})
	if denyReason(n) != ReasonNoLicense {
		t.Fatalf("certificate for another deployment accepted: %s", denyReason(n))
	}
	m, _ := newTestNode(t, "", now)
	m.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: now * 1000, NewestCertificate: c.Raw()})
	m.Apply(&Update{ClusterID: other, MasterNowUnixMs: now * 1000})
	if denyReason(m) != ReasonNoLicense {
		t.Fatalf("moving to another cluster must drop the old certificate: %s", denyReason(m))
	}
}

func TestNodeCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFileName)
	c := longLived(t)
	n, _ := newTestNode(t, path, t2026)
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: t2026 * 1000, NewestCertificate: c.Raw(), InternalReadKey: bytes.Repeat([]byte{1}, 32)})
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache not written with 0600: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("AQEB")) {
		t.Fatal("internal key must not be cached")
	}

	cached := NewNode(testTrust(t), path)
	if !allow(cached) {
		t.Fatalf("a cached certificate must allow reads before the first frame: %s", denyReason(cached))
	}
	if cached.InternalKey() != nil {
		t.Fatal("internal key restored from cache")
	}

	for name, content := range map[string]string{
		"not json":       "{",
		"tampered":       `{"newest":"` + strings.Replace(c.Raw(), ".", ".e", 1) + `","prev":"","cluster_id":"` + testDeployment + `"}`,
		"wrong cluster":  `{"newest":"` + c.Raw() + `","prev":"","cluster_id":"00000000-0000-4000-8000-000000000000"}`,
		"untrusted root": `{"newest":"` + c.Raw() + `","prev":"","cluster_id":"` + testDeployment + `"}`,
	} {
		os.WriteFile(path, []byte(content), 0600)
		trust := testTrust(t)
		if name == "untrusted root" {
			trust, _ = NewTrustStore(nil, nil)
		}
		if r := denyReason(NewNode(trust, path)); r != ReasonNotReady {
			t.Errorf("%s cache: got %s, want NOT_READY", name, r)
		}
	}
}

func TestNodeClockUsesMilliseconds(t *testing.T) {
	expiry := int64(t2026 + 200)
	n, ft := newTestNode(t, "", 0)
	c := certWith(t, 1, t2026, expiry, FeatureDataRead)
	// The master read its clock 900ms into second expiry-1.
	n.Apply(&Update{ClusterID: testDeployment, MasterNowUnixMs: (expiry-1)*1000 + 900, NewestCertificate: c.Raw()})
	if !allow(n) {
		t.Fatal("still valid at expiry-1")
	}
	ft.add(100 * time.Millisecond)
	n.Tick()
	if denyReason(n) != ReasonExpired {
		t.Fatalf("expiry must be seen 100ms later, not a second later: %s", denyReason(n))
	}
}

func TestNodeLogDeniedRateLimit(t *testing.T) {
	n := NewNode(testTrust(t), "")
	n.guard.clock.Store(1000)
	n.LogDenied("volume_http", ReasonNoLicense)
	if n.lastDenyLog.Load() != 1000 || n.deniedSinceLog.Load() != 0 {
		t.Fatal("first denial not logged")
	}
	n.guard.clock.Store(1059)
	n.LogDenied("volume_http", ReasonNoLicense)
	n.LogDenied("volume_http", ReasonNoLicense)
	if n.lastDenyLog.Load() != 1000 || n.deniedSinceLog.Load() != 2 {
		t.Fatal("denials within a minute were logged")
	}
	n.guard.clock.Store(1060)
	n.LogDenied("volume_http", ReasonNoLicense)
	if n.lastDenyLog.Load() != 1060 || n.deniedSinceLog.Load() != 0 {
		t.Fatal("next minute not logged")
	}
}
