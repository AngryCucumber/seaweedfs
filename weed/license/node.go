package license

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

const (
	nodeTickInterval  = 500 * time.Millisecond
	denyLogInterval   = 60
	maxCacheFileBytes = 4 * MaxCertificateBytes
	// CacheFileName is the node cache under the volume's first -dir or the
	// filer's -defaultStoreDir.
	CacheFileName = "license-cache.json"
)

// Update is what a master sends a node on the heartbeat or KeepConnected
// stream (master_pb.LicenseUpdate).
type Update struct {
	NewestCertificate string
	PrevCertificate   string
	MasterNowUnixMs   int64
	ClusterID         string
	// MasterDenyReason is empty, CLOCK_SUSPECT or NODE_REJECTED.
	MasterDenyReason string
	InternalReadKey  []byte
	StateVersion     uint64
}

// Node keeps the license state of a volume server or filer and owns the
// guard its read paths consult.
type Node struct {
	trust     *TrustStore
	cachePath string
	guard     *Guard
	clock     nodeClock

	mu         sync.Mutex
	certs      Certs
	clusterID  string
	received   bool
	masterDeny Reason
	rejected   bool

	// lastDenyLog and deniedSinceLog rate-limit LogDenied.
	lastDenyLog    atomic.Int64
	deniedSinceLog atomic.Int64
}

// NewNode creates a node license state and loads cachePath if present; an
// empty cachePath disables caching.
func NewNode(trust *TrustStore, cachePath string) *Node {
	n := &Node{trust: trust, cachePath: cachePath, guard: newGuard(), clock: nodeClock{timeNow: time.Now}}
	if cachePath != "" {
		if err := n.loadCache(); err != nil && !os.IsNotExist(err) {
			glog.Warningf("license: ignore cache %s: %v", cachePath, err)
		}
	}
	now := n.clock.tick()
	n.guard.clock.Store(now)
	n.publishLocked(now)
	return n
}

// Guard returns the guard for read paths.
func (n *Node) Guard() *Guard { return n.guard }

// Run ticks the node clock until stop is closed.
func (n *Node) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(nodeTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			n.Tick()
		}
	}
}

// Tick advances the clock and switches the snapshot when the certificate
// in effect changes, e.g. when a pending certificate starts.
func (n *Node) Tick() {
	now := n.clock.tick()
	n.guard.clock.Store(now)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.certs.Effective(now) != n.guard.snap.Load().cert {
		n.publishLocked(now)
	}
}

// Apply applies a master update. Time anchor, deny reason and internal key
// apply on every update; certificates replace the local ones only by the
// Certs.Compare rules, and only after verification.
func (n *Node) Apply(u *Update) {
	if u.MasterNowUnixMs > 0 {
		n.clock.anchor(u.MasterNowUnixMs)
	}
	now := n.clock.tick()
	n.guard.clock.Store(now)
	n.guard.setKey(u.InternalReadKey)

	n.mu.Lock()
	changed := !n.received
	n.received = true
	n.rejected = false
	n.masterDeny = Reason(u.MasterDenyReason)
	if u.ClusterID != "" && u.ClusterID != n.clusterID {
		if n.certs.Newest != nil && n.certs.Newest.Claims.DeploymentID != u.ClusterID {
			glog.Warningf("license: drop certificates of deployment %s, master reports %s", n.certs.Newest.Claims.DeploymentID, u.ClusterID)
			n.certs = Certs{}
		}
		n.clusterID = u.ClusterID
		changed = true
	}
	if incoming, err := n.parseCerts(u.NewestCertificate, u.PrevCertificate); err != nil {
		glog.Warningf("license: ignore certificates from master: %v", err)
	} else if replace, err := n.certs.Compare(incoming); err != nil {
		glog.Warningf("license: keep local certificates: %v", err)
	} else if replace {
		n.certs = incoming
		changed = true
	}
	n.publishLocked(now)
	cache := n.cacheLocked()
	n.mu.Unlock()

	if changed && n.cachePath != "" {
		if err := writeFileAtomic(n.cachePath, cache); err != nil {
			glog.Warningf("license: write cache %s: %v", n.cachePath, err)
		}
	}
}

// Reject marks this node as refused by the master's node quota. Reads are
// denied with NODE_REJECTED until the next Apply. It reports whether the
// node was not rejected before, so callers log once per rejection.
func (n *Node) Reject() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rejected {
		return false
	}
	n.rejected = true
	n.publishLocked(n.guard.Now())
	return true
}

// LogDenied logs a denied read at most once a minute per node; the other
// denials are counted into the next report.
func (n *Node) LogDenied(path string, reason Reason) {
	now := n.guard.Now()
	last := n.lastDenyLog.Load()
	if now-last < denyLogInterval || !n.lastDenyLog.CompareAndSwap(last, now) {
		n.deniedSinceLog.Add(1)
		return
	}
	glog.Warningf("license: denied %s read with %s (%d more since the last report)", path, reason, n.deniedSinceLog.Swap(0))
}

// InternalKey returns a copy of internal_read_key, or nil.
func (n *Node) InternalKey() []byte {
	if k := n.guard.key.Load(); k != nil {
		return append([]byte(nil), k[:]...)
	}
	return nil
}

func (n *Node) parseCerts(newest, prev string) (Certs, error) {
	c, err := parseCertPair(n.trust, newest, prev)
	if err != nil {
		return Certs{}, err
	}
	for _, cert := range []*Certificate{c.Newest, c.Prev} {
		if cert != nil && n.clusterID != "" && cert.Claims.DeploymentID != n.clusterID {
			return Certs{}, fmt.Errorf("certificate is for deployment %s, not %s", cert.Claims.DeploymentID, n.clusterID)
		}
	}
	return c, nil
}

// publishLocked rebuilds the snapshot. Deny precedence follows contract 2.1.
func (n *Node) publishLocked(now int64) {
	eff := n.certs.Effective(now)
	var reason Reason
	switch {
	case n.rejected || n.masterDeny == ReasonNodeRejected:
		reason = ReasonNodeRejected
	case !n.received && n.certs.Newest == nil:
		reason = ReasonNotReady
	case n.masterDeny != "":
		reason = n.masterDeny
	case n.certs.Newest == nil:
		reason = ReasonNoLicense
	case eff == nil:
		reason = ReasonNotYetValid
	}
	n.guard.snap.Store(newSnapshot(reason, eff, n.clusterID))
}

type nodeCache struct {
	Newest    string `json:"newest"`
	Prev      string `json:"prev"`
	ClusterID string `json:"cluster_id"`
}

func (n *Node) cacheLocked() []byte {
	c := nodeCache{ClusterID: n.clusterID}
	if n.certs.Newest != nil {
		c.Newest = n.certs.Newest.Raw()
	}
	if n.certs.Prev != nil {
		c.Prev = n.certs.Prev.Raw()
	}
	data, _ := json.Marshal(c)
	return data
}

func (n *Node) loadCache() error {
	info, err := os.Stat(n.cachePath)
	if err != nil {
		return err
	}
	if info.Size() > maxCacheFileBytes {
		return fmt.Errorf("cache larger than %d bytes", maxCacheFileBytes)
	}
	data, err := os.ReadFile(n.cachePath)
	if err != nil {
		return err
	}
	var c nodeCache
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	n.clusterID = c.ClusterID
	certs, err := n.parseCerts(c.Newest, c.Prev)
	if err != nil {
		n.clusterID = ""
		return err
	}
	n.certs = certs
	return nil
}

// writeFileAtomic replaces path with data so readers see the old or the
// new content, never a partial file. It returns only once the new content
// and its directory entry are on disk, so callers may count the write as
// durable.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".license-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0600)
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if err = syncDir(dir); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// syncDir flushes the entries of dir to disk. Windows cannot sync a
// directory, and file systems that do not support it report EINVAL or
// ENOTSUP; there a rename is as durable as the file system makes it.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// maxAnchorLag bounds how far the master clock may run behind the local
// wall clock. A forged or rolled back master time could otherwise keep an
// expired certificate in effect (plan 4.3).
const maxAnchorLag = 5 * time.Minute

// nodeClock follows the master's clock: master_now_unix_ms plus monotonic
// time elapsed since it arrived, in whole seconds, but never earlier than
// the local wall clock minus maxAnchorLag. Before the first anchor it uses
// the local wall clock. Milliseconds keep expiry detection within the tick
// interval plus network delay.
type nodeClock struct {
	mu       sync.Mutex
	timeNow  func() time.Time
	anchored bool
	anchorMs int64
	anchorAt time.Time
	// lagging is set while the local floor overrides the master time.
	lagging bool
}

func (c *nodeClock) anchor(masterNowUnixMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchored = true
	c.anchorMs = masterNowUnixMs
	c.anchorAt = c.timeNow()
}

func (c *nodeClock) tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.timeNow()
	if !c.anchored {
		return now.Unix()
	}
	masterMs := c.anchorMs + now.Sub(c.anchorAt).Milliseconds()
	floorMs := now.Add(-maxAnchorLag).UnixMilli()
	lagging := masterMs < floorMs
	if lagging != c.lagging {
		c.lagging = lagging
		if lagging {
			glog.Warningf("license: master time %s is more than %v behind the local clock %s; using the local clock minus %v",
				time.UnixMilli(masterMs).UTC().Format(time.RFC3339), maxAnchorLag, now.UTC().Format(time.RFC3339), maxAnchorLag)
		} else {
			glog.Infof("license: master time is within %v of the local clock again", maxAnchorLag)
		}
	}
	if lagging {
		return floorMs / 1000
	}
	return masterMs / 1000
}
