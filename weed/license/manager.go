package license

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

const (
	// MaxRequestBytes bounds import and validate request bodies.
	MaxRequestBytes = 96 * 1024

	peerRPCTimeout          = 3 * time.Second
	lockedOpTimeout         = 10 * time.Second
	retryInterval           = 5 * time.Second
	ledgerSyncTimeout       = 5 * time.Second
	managerTickInterval     = time.Second
	watermarkPersistSeconds = 60
	validatePerMinute       = 10
	commitRounds            = 3
)

// Membership is a raft configuration. Ids are master addresses.
type Membership struct {
	Voters    []string
	Nonvoters []string
	Index     uint64
}

func (ms Membership) all() []string {
	return append(append([]string{}, ms.Voters...), ms.Nonvoters...)
}

func (ms Membership) isVoter(id string) bool { return contains(ms.Voters, id) }

func (ms Membership) has(id string) bool { return ms.isVoter(id) || contains(ms.Nonvoters, id) }

func (ms Membership) size() int64 { return int64(len(ms.Voters) + len(ms.Nonvoters)) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// majority reports whether ok holds floor(n/2)+1 of the voters.
func majority(voters []string, ok map[string]bool) bool {
	n := 0
	for _, v := range voters {
		if ok[v] {
			n++
		}
	}
	return len(voters) > 0 && n >= len(voters)/2+1
}

// Raft is the manager's view of the master raft group. Implementations
// must not call back into the manager.
type Raft interface {
	IsLeader() bool
	// Leader returns the leader's raft id, or "" while unknown.
	Leader() string
	Term() uint64
	Membership() (Membership, error)
	// AddNonvoter, AddVoter and RemoveServer wait until the change commits.
	AddNonvoter(id, address string) error
	AddVoter(id, address string) error
	RemoveServer(id string) error
}

// LeaderInfo identifies the sender of SyncLicense and SyncLedger.
type LeaderInfo struct {
	Address     string
	Term        uint64
	ConfigIndex uint64
}

// Peers sends license RPCs to other masters.
type Peers interface {
	GetLicense(ctx context.Context, peer string) (State, error)
	GetLedger(ctx context.Context, peer string) (LedgerSnapshot, error)
	SyncLicense(ctx context.Context, peer string, from LeaderInfo, s State) (State, error)
	SyncLedger(ctx context.Context, peer string, from LeaderInfo, l LedgerSnapshot) error
}

// ManagerOption configures a Manager.
type ManagerOption struct {
	// Dir is <mdir>/license; empty keeps the state in memory only
	// (master.follower, which never joins raft).
	Dir string
	// Self is this master's raft id.
	Self  string
	Trust *TrustStore
	Raft  Raft
	Peers Peers
	// LicenseFile is -license.file; it is applied to the local state on start.
	LicenseFile string
	// Connected lists the volume servers and filers registered on this
	// master right now. A reconcile keeps them online: streams that
	// survived a leadership change do not register again.
	Connected func() []LedgerEntry
	// Now replaces the wall clock in tests.
	Now func() time.Time
}

// Manager is the license state of one master (plan 3.3, 3.4, 4.5). Only the
// leader imports; it commits license state to a voter majority, reconciles
// after every election before it admits nodes, and replicates the member
// ledger in batches. Followers accept state only from the current leader
// and term.
//
// opMu serializes the four operations that wait for peers while excluding
// each other: import, reconcile, master join and master removal. mu guards
// the state and is never held across an RPC or a raft call, so node
// admission only waits for microseconds.
type Manager struct {
	self      string
	stateFile string
	trust     *TrustStore
	raft      Raft
	peers     Peers
	connected func() []LedgerEntry
	timeNow   func() time.Time

	opMu      sync.Mutex
	persistMu sync.Mutex

	mu        sync.Mutex
	clusterID string
	certs     Certs
	key       []byte
	clock     ClockState
	ledger    *Ledger
	ledgerVer LedgerVersion
	// diskStale is set while the last state.json write failed: memory is
	// ahead of the disk, and nothing may be acknowledged until a write
	// succeeds (see ensureDurable).
	diskStale bool
	// ledgerEpoch is the term part of the ledger versions this master
	// writes as leader; epochFor is the raft term+1 it was chosen for, 0 for
	// none (see ledgerEpochFor).
	ledgerEpoch, epochFor uint64
	// rev changes with the cluster id, certificates or key.
	rev uint64
	// acked maps a peer to the rev it confirmed during ackedTerm (leader only).
	acked      map[string]uint64
	ackedTerm  uint64
	denyReason Reason
	// ledgerDirty: changes without a version yet; ledgerPending: the
	// latest version is not confirmed by a voter majority.
	ledgerDirty    bool
	ledgerPending  bool
	ledgerSending  bool
	lastLedgerSend time.Time
	lastPush       time.Time
	lastReconcile  time.Time
	lastClockSave  time.Time
	validateTokens float64
	validateAt     time.Time
	// conns counts the live registration streams of each node admitted by
	// this master. A node goes offline in the ledger when its last stream
	// ends, so a quick reconnect whose old stream closes late keeps it
	// online.
	conns map[ledgerKey]int
	// rejectLogAt and rejectsSinceLog limit refused registrations in the
	// log to one line a minute.
	rejectLogAt     time.Time
	rejectsSinceLog int

	// reconciled is term+1 of the last completed reconcile, 0 for none.
	reconciled   atomic.Uint64
	reconcileRun atomic.Bool
	pushRun      atomic.Bool
	// version is LicenseUpdate.state_version.
	version atomic.Uint64
}

// NewManager loads <dir>/state.json and applies opt.LicenseFile.
func NewManager(opt ManagerOption) (*Manager, error) {
	m := &Manager{
		self:           opt.Self,
		trust:          opt.Trust,
		raft:           opt.Raft,
		peers:          opt.Peers,
		connected:      opt.Connected,
		timeNow:        opt.Now,
		ledger:         NewLedger(nil),
		acked:          map[string]uint64{},
		conns:          map[ledgerKey]int{},
		validateTokens: validatePerMinute,
	}
	if m.timeNow == nil {
		m.timeNow = time.Now
	}
	m.version.Store(1)
	if opt.Dir != "" {
		m.stateFile = filepath.Join(opt.Dir, StateFileName)
		if err := os.MkdirAll(opt.Dir, 0700); err != nil {
			return nil, err
		}
		if err := m.load(); err != nil {
			return nil, err
		}
	}
	if opt.LicenseFile != "" {
		if err := m.applyLicenseFile(opt.LicenseFile); err != nil {
			return nil, err
		}
	}
	m.refreshClock(m.now())
	return m, nil
}

func (m *Manager) now() int64 { return m.timeNow().Unix() }

func (m *Manager) load() error {
	s, err := readStateFile(m.stateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	m.clusterID = s.ClusterID
	m.key = decodeInternalKey(s.InternalReadKey)
	m.clock = ClockState{Watermark: s.Watermark, RepairSeq: s.RepairSeq}
	if s.Ledger != nil {
		m.ledger = NewLedger(s.Ledger.Entries)
		m.ledgerVer = s.Ledger.LedgerVersion
	}
	certs, err := parseCertPair(m.trust, s.Newest, s.Prev)
	switch {
	case err != nil:
		glog.Errorf("license: ignore certificates in %s: %v", m.stateFile, err)
	case certs.Newest != nil && certs.Newest.Claims.DeploymentID != m.clusterID:
		glog.Errorf("license: ignore certificates in %s: deployment %s is not cluster %s", m.stateFile, certs.Newest.Claims.DeploymentID, m.clusterID)
	default:
		m.certs = certs
	}
	return nil
}

// applyLicenseFile imports -license.file into the local state before raft
// starts (contract 5.1). A file that is already applied or superseded is
// skipped, so the flag can stay on the command line.
func (m *Manager) applyLicenseFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("-license.file: %v", err)
	}
	c, err := ParseCertificate(bytes.TrimSpace(raw), m.trust)
	if err != nil {
		return fmt.Errorf("-license.file %s: %v", path, err)
	}
	if m.clusterID == "" {
		return fmt.Errorf("-license.file %s: this master has no cluster_id; run `weed license.init -mdir=<mdir> -clusterId=%s` first", path, c.Claims.DeploymentID)
	}
	if n := m.certs.Newest; n != nil && (c.SameBytes(n) || c.Claims.Sequence < n.Claims.Sequence) {
		glog.V(0).Infof("license: -license.file sequence %d is already applied or superseded by %d", c.Claims.Sequence, n.Claims.Sequence)
		return nil
	}
	now := m.now()
	res, err := m.certs.CheckImport(c, m.clusterID, now, m.ledger.Usage())
	if err != nil {
		return fmt.Errorf("-license.file %s: %v", path, err)
	}
	m.certs = res.Certs
	m.clock = m.clock.RepairOnImport(c, now)
	m.rev++
	glog.V(0).Infof("license: applied -license.file %s sequence %d", c.Claims.LicenseID, c.Claims.Sequence)
	return m.persist()
}

// persist writes state.json. It must be called without mu.
func (m *Manager) persist() error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	s := &stateFile{
		ClusterID: m.clusterID,
		Newest:    rawOf(m.certs.Newest),
		Prev:      rawOf(m.certs.Prev),
		Watermark: m.clock.Watermark,
		RepairSeq: m.clock.RepairSeq,
		Ledger:    &LedgerSnapshot{LedgerVersion: m.ledgerVer, Entries: m.ledger.Entries()},
	}
	if len(m.key) == InternalKeySize {
		s.InternalReadKey = EncodeInternalToken(m.key)
	}
	m.lastClockSave = m.timeNow()
	m.mu.Unlock()
	if m.stateFile == "" {
		return nil
	}
	err := writeState(m.stateFile, s)
	m.mu.Lock()
	m.diskStale = err != nil
	m.mu.Unlock()
	return err
}

// writeState writes state.json; tests replace it to inject failures.
var writeState = writeStateFile

// ensureDurable rewrites state.json if the last write failed. A master
// confirms state to the leader, or counts itself as leader, only after it
// succeeds: otherwise a retry of an unchanged state would be confirmed
// while the disk still holds the old one.
func (m *Manager) ensureDurable() error {
	m.mu.Lock()
	stale := m.diskStale
	m.mu.Unlock()
	if !stale {
		return nil
	}
	return m.persist()
}

func (m *Manager) persistOrLog() {
	if err := m.persist(); err != nil {
		glog.Errorf("license: write %s: %v", m.stateFile, err)
	}
}

// bumpLocked records a change of the cluster id, certificates or key.
func (m *Manager) bumpLocked() {
	m.rev++
	m.version.Add(1)
}

func (m *Manager) stateLocked() State {
	return State{
		ClusterID:       m.clusterID,
		Newest:          rawOf(m.certs.Newest),
		Prev:            rawOf(m.certs.Prev),
		InternalReadKey: append([]byte(nil), m.key...),
		Clock:           m.clock,
	}
}

// ledgerForPushLocked returns the ledger under a version no peer has seen
// with different content: unsent changes get a fresh version, which stays
// pending until a batch commits it to a voter majority.
func (m *Manager) ledgerForPushLocked(term uint64) LedgerSnapshot {
	epoch := term
	if m.epochFor == term+1 {
		epoch = m.ledgerEpoch
	}
	if m.ledgerDirty || m.ledgerVer.Term != epoch {
		m.ledgerVer = nextLedgerVersion(m.ledgerVer, epoch)
		m.ledgerDirty, m.ledgerPending = false, true
	}
	return LedgerSnapshot{LedgerVersion: m.ledgerVer, Entries: m.ledger.Entries()}
}

// ledgerEpochFor returns the term part of the ledger versions a leader of
// raft term writes after its reconcile saw seen as the highest version. It
// is the raft term unless that went backwards: goraft deletes its log on
// every start, so after all masters restart the terms count from 0 again
// while state.json keeps the old ledger versions. The epoch then continues
// above the highest seen, so the new leader's versions still win.
func ledgerEpochFor(seen LedgerVersion, term uint64) uint64 {
	if seen.Term >= term {
		return seen.Term + 1
	}
	return term
}

func nextLedgerVersion(cur LedgerVersion, term uint64) LedgerVersion {
	if cur.Term == term {
		return LedgerVersion{Term: term, Revision: cur.Revision + 1}
	}
	return LedgerVersion{Term: term, Revision: 1}
}

// Run drives clock checks, reconcile retries, ledger batches and state
// pushes until stop is closed.
func (m *Manager) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(managerTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

func (m *Manager) tick() {
	now := m.now()
	m.refreshClock(now)
	m.mu.Lock()
	saveClock := m.timeNow().Sub(m.lastClockSave) >= watermarkPersistSeconds*time.Second
	m.mu.Unlock()
	if saveClock {
		m.persistOrLog()
	}

	if !m.raft.IsLeader() {
		return
	}
	term := m.raft.Term()
	if m.reconciled.Load() != term+1 {
		m.mu.Lock()
		due := m.timeNow().Sub(m.lastReconcile) >= retryInterval
		m.mu.Unlock()
		if due && m.reconcileRun.CompareAndSwap(false, true) {
			go func() {
				defer m.reconcileRun.Store(false)
				m.reconcile(term)
			}()
		}
		return
	}
	m.mu.Lock()
	if m.ledger.ExpireGrace(now) > 0 {
		m.ledgerDirty = true
	}
	m.mu.Unlock()
	m.replicateLedger(term)
	m.pushPending(term)
}

// refreshClock advances the watermark and switches CLOCK_SUSPECT.
func (m *Manager) refreshClock(now int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var deny Reason
	if m.clock.Suspect(now) {
		deny = ReasonClockSuspect
	} else {
		m.clock = m.clock.Advance(now)
	}
	if deny != m.denyReason {
		if deny != "" {
			glog.Errorf("license: wall clock %d is behind watermark %d; data reads are denied with %s", now, m.clock.Watermark, deny)
		} else {
			glog.Warningf("license: wall clock has passed the watermark again")
		}
		m.denyReason = deny
		m.version.Add(1)
	}
}

// Reconciling reports whether this master is leader but has not finished
// reconciling for the current term. Imports, membership changes and node
// registrations wait until it returns false.
func (m *Manager) Reconciling() bool {
	return m.raft.IsLeader() && m.reconciled.Load() != m.raft.Term()+1
}

// WaitReconciled waits until this master is a reconciled leader.
func (m *Manager) WaitReconciled(ctx context.Context) error {
	for {
		if m.raft.IsLeader() && !m.Reconciling() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// leaderTerm returns the current term if this master is a reconciled leader.
func (m *Manager) leaderTerm() (uint64, error) {
	if !m.raft.IsLeader() {
		return 0, errorf(CodeNotLeader, "leader is %q", m.raft.Leader())
	}
	term := m.raft.Term()
	if m.reconciled.Load() != term+1 {
		return 0, &Error{Code: CodeReconciling}
	}
	return term, nil
}

// reconcile runs the election reconcile of plan 3.3 item 5 and 3.4: read
// license state and ledger from a voter majority, take the highest of
// each, hand the recovered ledger over, and write both back to a voter
// majority before registrations open.
func (m *Manager) reconcile(term uint64) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.raft.IsLeader() || m.raft.Term() != term || m.reconciled.Load() == term+1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockedOpTimeout)
	defer cancel()
	err := m.reconcileLocked(ctx, term)
	m.mu.Lock()
	m.lastReconcile = m.timeNow()
	m.mu.Unlock()
	if err != nil {
		glog.Warningf("license: reconcile for term %d: %v; retry in %v", term, err, retryInterval)
		return
	}
	m.reconciled.Store(term + 1)
	glog.V(0).Infof("license: reconciled for term %d, registrations are open", term)
}

type peerReply struct {
	peer   string
	state  State
	ledger LedgerSnapshot
	err    error
}

func (m *Manager) reconcileLocked(ctx context.Context, term uint64) error {
	mem, err := m.raft.Membership()
	if err != nil {
		return err
	}
	replies := m.collect(ctx, without(mem.all(), m.self))
	answered := map[string]bool{m.self: true}
	for _, r := range replies {
		if r.err == nil {
			answered[r.peer] = true
		} else {
			glog.V(1).Infof("license: reconcile: %s: %v", r.peer, r.err)
		}
	}
	if !majority(mem.Voters, answered) {
		return fmt.Errorf("only %d of %d voters answered", countIn(mem.Voters, answered), len(mem.Voters))
	}

	now := m.now()
	m.mu.Lock()
	best := candidate{clusterID: m.clusterID, certs: m.certs, key: m.key, clock: m.clock}
	bestLedger := LedgerSnapshot{LedgerVersion: m.ledgerVer, Entries: m.ledger.Entries()}
	m.mu.Unlock()
	for _, r := range replies {
		if r.err != nil {
			continue
		}
		m.merge(&best, r.peer, r.state)
		if bestLedger.LedgerVersion.Less(r.ledger.LedgerVersion) {
			bestLedger = r.ledger
		}
	}
	if best.clusterID == "" {
		best.clusterID = newClusterID()
		glog.V(0).Infof("license: generated cluster_id %s", best.clusterID)
	}
	if len(best.key) != InternalKeySize {
		best.key = newInternalKey()
	}
	recovered := NewLedger(bestLedger.Entries)
	recovered.TakeOver(now)
	if m.connected != nil {
		for _, e := range m.connected() {
			recovered.markOnline(e.Type, e.Address, now)
		}
	}

	m.mu.Lock()
	if best.clusterID != m.clusterID || rawOf(best.certs.Newest) != rawOf(m.certs.Newest) ||
		rawOf(best.certs.Prev) != rawOf(m.certs.Prev) || !bytes.Equal(best.key, m.key) {
		m.clusterID, m.certs, m.key = best.clusterID, best.certs, best.key
		m.bumpLocked()
	}
	if best.clock.RepairSeq > m.clock.RepairSeq {
		m.clock = best.clock
	}
	m.ledger = recovered
	seen := maxVersion(m.ledgerVer, bestLedger.LedgerVersion)
	// A retry in the same term keeps its epoch unless it now sees a
	// version above it.
	if m.epochFor != term+1 || seen.Term > m.ledgerEpoch {
		m.ledgerEpoch, m.epochFor = ledgerEpochFor(seen, term), term+1
	}
	m.ledgerVer = nextLedgerVersion(seen, m.ledgerEpoch)
	m.ledgerDirty, m.ledgerPending = false, true
	st, rev := m.stateLocked(), m.rev
	led := LedgerSnapshot{LedgerVersion: m.ledgerVer, Entries: m.ledger.Entries()}
	m.mu.Unlock()
	if err := m.persist(); err != nil {
		return err
	}

	votersOK, failed, ok := m.commit(ctx, term, rev, &st, &led)
	if !ok {
		return fmt.Errorf("write-back reached %d voters %v, failed %v", len(votersOK), votersOK, failed)
	}
	m.mu.Lock()
	if m.ledgerVer == led.LedgerVersion {
		m.ledgerPending = false
	}
	m.mu.Unlock()
	m.mu.Lock()
	changed := m.rev != rev
	m.mu.Unlock()
	if changed {
		return fmt.Errorf("a peer holds newer license state; adopted it")
	}
	return nil
}

func maxVersion(a, b LedgerVersion) LedgerVersion {
	if a.Less(b) {
		return b
	}
	return a
}

func countIn(ids []string, set map[string]bool) int {
	n := 0
	for _, id := range ids {
		if set[id] {
			n++
		}
	}
	return n
}

// collect reads license state and ledger from peers in parallel.
func (m *Manager) collect(ctx context.Context, peers []string) []peerReply {
	replies := make([]peerReply, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, peerRPCTimeout)
			defer cancel()
			r := peerReply{peer: p}
			if r.state, r.err = m.peers.GetLicense(ctx, p); r.err == nil {
				r.ledger, r.err = m.peers.GetLedger(ctx, p)
			}
			replies[i] = r
		}(i, p)
	}
	wg.Wait()
	sort.Slice(replies, func(i, j int) bool { return replies[i].peer < replies[j].peer })
	return replies
}

type candidate struct {
	clusterID string
	certs     Certs
	key       []byte
	clock     ClockState
}

// merge folds a peer's state into the reconcile candidate by the Certs
// ordering. Clock state follows the newer repair only: a master keeps its
// own watermark otherwise.
func (m *Manager) merge(best *candidate, peer string, s State) {
	certs, err := parseCertPair(m.trust, s.Newest, s.Prev)
	if err == nil && certs.Newest != nil && certs.Newest.Claims.DeploymentID != s.ClusterID {
		err = fmt.Errorf("certificate is for deployment %s, not %s", certs.Newest.Claims.DeploymentID, s.ClusterID)
	}
	if err != nil {
		glog.Warningf("license: reconcile: ignore state of %s: %v", peer, err)
		return
	}
	if best.certs.Newest != nil && certs.Newest != nil && best.clusterID != s.ClusterID {
		glog.Errorf("license: reconcile: %s holds a license of deployment %s, this cluster is %s; ignored", peer, s.ClusterID, best.clusterID)
		return
	}
	replace, err := best.certs.Compare(certs)
	switch {
	case err != nil && codeOf(err) == CodeSequenceConflict:
		glog.Errorf("license: reconcile: %s: %v", peer, err)
		return
	case replace && certs.Newest.SameBytes(best.certs.Newest):
		best.certs = certs // a better Prev only
	case replace:
		best.clusterID, best.certs, best.key = s.ClusterID, certs, s.InternalReadKey
	case best.certs.Newest == nil && best.clusterID == "" && s.ClusterID != "":
		best.clusterID, best.key = s.ClusterID, s.InternalReadKey
	}
	if len(best.key) != InternalKeySize && len(s.InternalReadKey) == InternalKeySize && best.clusterID == s.ClusterID {
		best.key = s.InternalReadKey
	}
	if s.Clock.RepairSeq > best.clock.RepairSeq {
		best.clock = s.Clock
	}
}

func codeOf(err error) Code {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

type pushResult struct {
	ok     map[string]bool
	failed map[string]string
	ahead  []State
}

// push sends the license state and/or the ledger to targets in parallel.
// A peer counts only if it now holds exactly st; peers that are ahead are
// returned so the leader can catch up.
func (m *Manager) push(ctx context.Context, from LeaderInfo, targets []string, st *State, led *LedgerSnapshot) pushResult {
	res := pushResult{ok: map[string]bool{}, failed: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range targets {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			got, err := m.pushOne(ctx, from, p, st, led)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				res.failed[p] = err.Error()
			case st != nil && !holds(got, *st):
				res.failed[p] = "peer holds other license state"
				res.ahead = append(res.ahead, got)
			default:
				res.ok[p] = true
			}
		}(p)
	}
	wg.Wait()
	return res
}

func (m *Manager) pushOne(ctx context.Context, from LeaderInfo, peer string, st *State, led *LedgerSnapshot) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, peerRPCTimeout)
	defer cancel()
	var got State
	if st != nil {
		var err error
		if got, err = m.peers.SyncLicense(ctx, peer, from, *st); err != nil {
			return got, err
		}
	}
	if led != nil {
		if err := m.peers.SyncLedger(ctx, peer, from, *led); err != nil {
			return got, err
		}
	}
	return got, nil
}

func holds(got, want State) bool {
	return got.ClusterID == want.ClusterID && got.Newest == want.Newest && got.Prev == want.Prev &&
		bytes.Equal(got.InternalReadKey, want.InternalReadKey)
}

// commit pushes to every member and follows configuration changes while it
// runs (plan 3.3 item 10). It reports the voters of the latest
// configuration that hold the data and whether they are a majority.
func (m *Manager) commit(ctx context.Context, term, rev uint64, st *State, led *LedgerSnapshot) (votersOK, failed []string, ok bool) {
	attempted := map[string]bool{m.self: true}
	okSet := map[string]bool{}
	failedSet := map[string]string{}
	if err := m.ensureDurable(); err != nil {
		failedSet[m.self] = "write state: " + err.Error()
	} else {
		okSet[m.self] = true
	}
	votersOK, failed = []string{}, []string{}
	var mem Membership
	for round := 0; round < commitRounds; round++ {
		cur, err := m.raft.Membership()
		if err != nil {
			glog.Warningf("license: read raft configuration: %v", err)
			break
		}
		mem = cur
		var targets []string
		for _, id := range mem.all() {
			if !attempted[id] {
				attempted[id] = true
				targets = append(targets, id)
			}
		}
		if len(targets) == 0 {
			break
		}
		res := m.push(ctx, LeaderInfo{Address: m.self, Term: term, ConfigIndex: mem.Index}, targets, st, led)
		for p := range res.ok {
			okSet[p] = true
		}
		for p, e := range res.failed {
			failedSet[p] = e
		}
		if len(res.ahead) > 0 {
			m.adoptAhead(res.ahead)
		}
	}
	for _, v := range mem.Voters {
		if okSet[v] {
			votersOK = append(votersOK, v)
		}
	}
	for p, e := range failedSet {
		if mem.has(p) {
			failed = append(failed, p)
			glog.Warningf("license: sync to %s: %s", p, e)
		}
	}
	sort.Strings(votersOK)
	sort.Strings(failed)
	if st != nil {
		m.mu.Lock()
		if m.ackedTerm != term {
			m.acked, m.ackedTerm = map[string]uint64{}, term
		}
		for p := range okSet {
			m.acked[p] = rev
		}
		m.mu.Unlock()
	}
	return votersOK, failed, majority(mem.Voters, okSet)
}

// adoptAhead takes over newer license state that a peer reported.
func (m *Manager) adoptAhead(states []State) {
	changed := false
	for _, s := range states {
		certs, err := parseCertPair(m.trust, s.Newest, s.Prev)
		if err != nil || certs.Newest == nil || certs.Newest.Claims.DeploymentID != s.ClusterID {
			continue
		}
		m.mu.Lock()
		if replace, err := m.certs.Compare(certs); err == nil && replace && (m.certs.Newest == nil || m.clusterID == s.ClusterID) {
			if certs.Newest.SameBytes(m.certs.Newest) {
				glog.Warningf("license: a peer holds previous certificate sequence %d under sequence %d; adopted it", certs.Prev.Claims.Sequence, certs.Newest.Claims.Sequence)
			} else {
				glog.Warningf("license: a peer holds license sequence %d; adopted it", certs.Newest.Claims.Sequence)
				if len(s.InternalReadKey) == InternalKeySize {
					m.key = append([]byte(nil), s.InternalReadKey...)
				}
			}
			m.clusterID, m.certs = s.ClusterID, certs
			m.bumpLocked()
			changed = true
		}
		m.mu.Unlock()
	}
	if changed {
		m.persistOrLog()
	}
}

// replicateLedger sends one ledger batch at most every retryInterval with
// at most one batch in flight (plan 3.4). RPCs run outside mu; a result
// counts only if its version is still the latest sent in this term.
func (m *Manager) replicateLedger(term uint64) {
	m.mu.Lock()
	if !(m.ledgerDirty || m.ledgerPending) || m.ledgerSending || m.timeNow().Sub(m.lastLedgerSend) < retryInterval {
		m.mu.Unlock()
		return
	}
	led := m.ledgerForPushLocked(term)
	m.ledgerSending, m.lastLedgerSend = true, m.timeNow()
	m.mu.Unlock()
	go func() {
		m.persistOrLog()
		ctx, cancel := context.WithTimeout(context.Background(), ledgerSyncTimeout)
		defer cancel()
		_, failed, ok := m.commit(ctx, term, 0, nil, &led)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.ledgerSending = false
		switch {
		case m.ledgerVer != led.LedgerVersion:
			// A newer version was assigned meanwhile; it is pending itself.
		case ok:
			m.ledgerPending = false
		default:
			// Not confirmed: the next batch carries the same entries under a new version.
			m.ledgerDirty = true
			glog.Warningf("license: ledger %d/%d not confirmed by a voter majority, failed %v", led.Term, led.Revision, failed)
		}
	}()
}

// pushPending re-sends the license state to members that have not
// confirmed the current revision, e.g. after a partial commit or restart.
func (m *Manager) pushPending(term uint64) {
	m.mu.Lock()
	if m.timeNow().Sub(m.lastPush) < retryInterval {
		m.mu.Unlock()
		return
	}
	m.lastPush = m.timeNow()
	if m.ackedTerm != term {
		m.acked, m.ackedTerm = map[string]uint64{}, term
	}
	st, rev := m.stateLocked(), m.rev
	acked := make(map[string]bool, len(m.acked))
	for p, r := range m.acked {
		acked[p] = r == rev
	}
	m.mu.Unlock()
	mem, err := m.raft.Membership()
	if err != nil {
		return
	}
	var targets []string
	for _, id := range mem.all() {
		if id != m.self && !acked[id] {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 || !m.pushRun.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.pushRun.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), peerRPCTimeout)
		defer cancel()
		res := m.push(ctx, LeaderInfo{Address: m.self, Term: term, ConfigIndex: mem.Index}, targets, &st, nil)
		if len(res.ahead) > 0 {
			m.adoptAhead(res.ahead)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ackedTerm == term {
			for p := range res.ok {
				m.acked[p] = rev
			}
		}
	}()
}

// ImportOutcome is the result of an import (POST /license/import).
type ImportOutcome struct {
	Accepted     bool                `json:"accepted"`
	Unchanged    bool                `json:"unchanged"`
	EffectiveNow bool                `json:"effective_now"`
	StateVersion uint64              `json:"state_version"`
	VotersOK     []string            `json:"voters_ok"`
	PeersFailed  []string            `json:"peers_failed"`
	Certificate  *CertificateSummary `json:"certificate"`
}

// ValidateOutcome is the result of POST /license/validate.
type ValidateOutcome struct {
	Valid        bool                `json:"valid"`
	Unchanged    bool                `json:"unchanged"`
	EffectiveNow bool                `json:"effective_now"`
	Certificate  *CertificateSummary `json:"certificate"`
}

// Import imports a certificate on the leader (contract 1.5) and commits it
// to a voter majority. On CodePartialCommit the outcome is returned too:
// the leader keeps the state and a retry of the same bytes only pushes.
func (m *Manager) Import(raw []byte) (*ImportOutcome, error) {
	c, err := ParseCertificate(bytes.TrimSpace(raw), m.trust)
	if err != nil {
		return nil, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	term, err := m.leaderTerm()
	if err != nil {
		return nil, err
	}
	mem, err := m.raft.Membership()
	if err != nil {
		return nil, err
	}
	now := m.now()
	m.mu.Lock()
	used := m.ledger.Usage()
	used.Masters = mem.size()
	res, err := m.certs.CheckImport(c, m.clusterID, now, used)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if !res.Unchanged {
		m.certs = res.Certs
		m.clock = m.clock.RepairOnImport(c, now)
		m.bumpLocked()
	}
	st, rev := m.stateLocked(), m.rev
	m.mu.Unlock()
	if !res.Unchanged {
		if err := m.persist(); err != nil {
			return nil, err
		}
		glog.V(0).Infof("license: imported %s sequence %d", c.Claims.LicenseID, c.Claims.Sequence)
	} else if err := m.ensureDurable(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), lockedOpTimeout)
	defer cancel()
	votersOK, failed, ok := m.commit(ctx, term, rev, &st, nil)
	out := &ImportOutcome{
		Accepted:     ok,
		Unchanged:    res.Unchanged,
		EffectiveNow: c.Claims.NotBefore <= now,
		StateVersion: m.version.Load(),
		VotersOK:     votersOK,
		PeersFailed:  failed,
		Certificate:  c.Summary(),
	}
	if !ok {
		return out, errorf(CodePartialCommit, "%d of %d voters confirmed; failed: %s", len(votersOK), len(mem.Voters), strings.Join(failed, ","))
	}
	return out, nil
}

// Validate runs the import checks without changing anything. It is rate
// limited per process.
func (m *Manager) Validate(raw []byte) (*ValidateOutcome, error) {
	if !m.takeValidateToken() {
		return nil, errorf(CodeRateLimited, "at most %d validations per minute", validatePerMinute)
	}
	c, err := ParseCertificate(bytes.TrimSpace(raw), m.trust)
	if err != nil {
		return nil, err
	}
	mem, err := m.raft.Membership()
	if err != nil {
		return nil, err
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	used := m.ledger.Usage()
	used.Masters = mem.size()
	res, err := m.certs.CheckImport(c, m.clusterID, now, used)
	if err != nil {
		return nil, err
	}
	return &ValidateOutcome{Valid: true, Unchanged: res.Unchanged, EffectiveNow: c.Claims.NotBefore <= now, Certificate: c.Summary()}, nil
}

func (m *Manager) takeValidateToken() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.timeNow()
	m.validateTokens = min(validatePerMinute, m.validateTokens+t.Sub(m.validateAt).Minutes()*validatePerMinute)
	m.validateAt = t
	if m.validateTokens < 1 {
		return false
	}
	m.validateTokens--
	return true
}

// checkSender accepts SyncLicense and SyncLedger only from the current
// leader in the current term (plan 3.3 item 3).
func (m *Manager) checkSender(from LeaderInfo) error {
	leader, term := m.raft.Leader(), m.raft.Term()
	if from.Address == m.self || from.Address != leader || from.Term != term {
		return errorf(CodeNotLeader, "sender %s term %d is not the current leader %q term %d", from.Address, from.Term, leader, term)
	}
	return nil
}

// State returns the local license state (GetLicense with include_state).
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

// Ledger returns the local ledger snapshot (GetLedger).
func (m *Manager) Ledger() LedgerSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return LedgerSnapshot{LedgerVersion: m.ledgerVer, Entries: m.ledger.Entries()}
}

// ApplySync applies SyncLicense from the leader and returns the local
// state afterwards. A follower that is ahead keeps its state and returns
// it so the leader can catch up.
func (m *Manager) ApplySync(from LeaderInfo, in State) (State, error) {
	if err := m.checkSender(from); err != nil {
		return State{}, err
	}
	incoming, err := parseCertPair(m.trust, in.Newest, in.Prev)
	if err != nil {
		return State{}, err
	}
	if incoming.Newest != nil && incoming.Newest.Claims.DeploymentID != in.ClusterID {
		return State{}, errorf(CodeDeploymentMismatch, "certificate is for deployment %s, not %s", incoming.Newest.Claims.DeploymentID, in.ClusterID)
	}
	m.mu.Lock()
	changed := false
	switch {
	case in.ClusterID == "" || in.ClusterID == m.clusterID:
	case m.certs.Newest != nil && incoming.Newest != nil:
		m.mu.Unlock()
		glog.Errorf("license: leader %s sends deployment %s, this master holds a license of %s", from.Address, in.ClusterID, m.clusterID)
		return State{}, errorf(CodeDeploymentMismatch, "this master holds a license of deployment %s", m.clusterID)
	case m.certs.Newest != nil:
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	default:
		m.clusterID = in.ClusterID
		changed = true
	}
	replace, err := m.certs.Compare(incoming)
	switch {
	case err != nil && codeOf(err) == CodeSequenceNotIncreased:
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	case err != nil:
		m.mu.Unlock()
		glog.Errorf("license: leader %s: %v", from.Address, err)
		return State{}, err
	case replace:
		m.certs = incoming
		changed = true
	}
	if rawOf(m.certs.Newest) == in.Newest && len(in.InternalReadKey) == InternalKeySize && !bytes.Equal(m.key, in.InternalReadKey) {
		m.key = append([]byte(nil), in.InternalReadKey...)
		changed = true
	}
	clock := m.clock.Merge(in.Clock)
	repaired := clock.RepairSeq != m.clock.RepairSeq
	m.clock = clock
	if changed {
		m.bumpLocked()
	}
	st := m.stateLocked()
	m.mu.Unlock()
	if changed || repaired {
		if err := m.persist(); err != nil {
			return State{}, err
		}
	} else if err := m.ensureDurable(); err != nil {
		return State{}, err
	}
	return st, nil
}

// ApplyLedgerSync applies SyncLedger from the leader. Versions order by
// (term, revision); an older version is refused and the same version must
// carry the same entries.
func (m *Manager) ApplyLedgerSync(from LeaderInfo, in LedgerSnapshot) error {
	if err := m.checkSender(from); err != nil {
		return err
	}
	m.mu.Lock()
	switch {
	case m.ledgerVer.Less(in.LedgerVersion):
		m.ledger = NewLedger(in.Entries)
		m.ledgerVer = in.LedgerVersion
	case m.ledgerVer == in.LedgerVersion:
		same := sameEntries(m.ledger.Entries(), NewLedger(in.Entries).Entries())
		m.mu.Unlock()
		if !same {
			glog.Errorf("license: ledger %d/%d from %s differs from the local copy", in.Term, in.Revision, from.Address)
			return fmt.Errorf("license: ledger %d/%d differs from the local copy", in.Term, in.Revision)
		}
		return m.ensureDurable()
	default:
		cur := m.ledgerVer
		m.mu.Unlock()
		return fmt.Errorf("license: ledger %d/%d is older than the local %d/%d", in.Term, in.Revision, cur.Term, cur.Revision)
	}
	m.mu.Unlock()
	return m.persist()
}

func sameEntries(a, b []LedgerEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CheckBootstrap is called before raft bootstraps a new configuration with
// peers masters (contract 5.1): without a certificate only a single master
// may bootstrap; with one, peers must fit max_master_nodes.
func (m *Manager) CheckBootstrap(peers int) error {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.certs.Newest == nil {
		if peers <= 1 {
			return nil
		}
		return errorf(CodeNodeLimitExceeded, "master %d/0 no license imported; run `weed license.init -mdir=<mdir>` on every master and start them with -license.file", peers)
	}
	if limit := m.certs.Limits(now).MaxMasterNodes; int64(peers) > limit {
		return errorf(CodeNodeLimitExceeded, "master %d/%d", peers, limit)
	}
	return nil
}

func (m *Manager) noLicenseHint(err *Error) error {
	if m.certs.Newest == nil {
		err.Message += " no license imported"
	}
	return err
}

// JoinMaster adds a master in two phases (plan 3.3 item 7): reserve a slot
// and add it as a nonvoter, push license state and ledger to it, and only
// then promote it to voter. On failure it stays a nonvoter.
func (m *Manager) JoinMaster(ctx context.Context, id, address string, voter bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	term, err := m.leaderTerm()
	if err != nil {
		return err
	}
	mem, err := m.raft.Membership()
	if err != nil {
		return err
	}
	if mem.isVoter(id) || (!voter && mem.has(id)) {
		return nil
	}
	if !mem.has(id) {
		now := m.now()
		m.mu.Lock()
		limit := m.certs.Limits(now).MaxMasterNodes
		if mem.size() >= limit {
			err := m.noLicenseHint(errorf(CodeNodeLimitExceeded, "master %d/%d", mem.size(), limit))
			m.mu.Unlock()
			return err
		}
		m.mu.Unlock()
		if err := m.raft.AddNonvoter(id, address); err != nil {
			return err
		}
		glog.V(0).Infof("license: added master %s as nonvoter", id)
	}
	if !voter {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, lockedOpTimeout)
	defer cancel()
	m.mu.Lock()
	st, rev := m.stateLocked(), m.rev
	led := m.ledgerForPushLocked(term)
	m.mu.Unlock()
	for {
		mem, err := m.raft.Membership()
		if err != nil {
			return err
		}
		res := m.push(ctx, LeaderInfo{Address: m.self, Term: term, ConfigIndex: mem.Index}, []string{id}, &st, &led)
		if res.ok[id] {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("master %s stays nonvoter: license state not confirmed: %s", id, res.failed[id])
		case <-time.After(500 * time.Millisecond):
		}
	}
	m.mu.Lock()
	if m.ackedTerm == term {
		m.acked[id] = rev
	}
	m.mu.Unlock()
	if err := m.raft.AddVoter(id, address); err != nil {
		return err
	}
	glog.V(0).Infof("license: promoted master %s to voter", id)
	return nil
}

// RemoveMaster removes a master. Before a voter is removed the license
// state and ledger are committed to a majority of the remaining voters,
// and remove runs only if neither the license state nor the raft
// configuration changed meanwhile (plan 3.3 item 8).
func (m *Manager) RemoveMaster(ctx context.Context, id string, remove func() error) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	term, err := m.leaderTerm()
	if err != nil {
		return err
	}
	mem, err := m.raft.Membership()
	if err != nil {
		return err
	}
	if !mem.isVoter(id) {
		return remove()
	}
	remaining := without(mem.Voters, id)
	m.mu.Lock()
	st, rev := m.stateLocked(), m.rev
	led := m.ledgerForPushLocked(term)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, lockedOpTimeout)
	defer cancel()
	res := m.push(ctx, LeaderInfo{Address: m.self, Term: term, ConfigIndex: mem.Index}, without(remaining, m.self), &st, &led)
	res.ok[m.self] = true
	if !majority(remaining, res.ok) {
		var failed []string
		for p := range res.failed {
			failed = append(failed, p)
		}
		sort.Strings(failed)
		return errorf(CodePartialCommit, "removing %s needs %d of the remaining %d voters to confirm the license state; failed: %s",
			id, len(remaining)/2+1, len(remaining), strings.Join(failed, ","))
	}
	after, err := m.raft.Membership()
	if err != nil {
		return err
	}
	m.mu.Lock()
	sameRev := m.rev == rev
	m.mu.Unlock()
	if after.Index != mem.Index || !sameRev {
		return errorf(CodeMembershipChanged, "license state or raft configuration changed while removing %s; retry", id)
	}
	if err := remove(); err != nil {
		return err
	}
	glog.V(0).Infof("license: removed master %s", id)
	return nil
}

// AdmitNode registers a volume server or filer under its quota (plan 3.4)
// when its stream opens on the leader. It never waits for peers. Each
// successful call must be paired with a DisconnectNode when the stream
// ends.
func (m *Manager) AdmitNode(t NodeType, addr string) error {
	if m.Reconciling() {
		return &Error{Code: CodeReconciling}
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ledger.Admit(t, addr, now, m.certs.Limits(now)); err != nil {
		if e, ok := err.(*Error); ok {
			err = m.noLicenseHint(e)
		}
		m.logRejectLocked(t, addr, err)
		return err
	}
	m.conns[ledgerKey{t, addr}]++
	m.ledgerDirty = true
	return nil
}

// logRejectLocked logs a refused registration at most once a minute.
func (m *Manager) logRejectLocked(t NodeType, addr string, err error) {
	now := m.timeNow()
	if now.Sub(m.rejectLogAt) < time.Minute {
		m.rejectsSinceLog++
		return
	}
	msg := strings.TrimPrefix(err.Error(), "license: ")
	if m.rejectsSinceLog > 0 {
		glog.Warningf("license: refused %s %s: %s (%d more refused registrations in the last minute)", t, addr, msg, m.rejectsSinceLog)
	} else {
		glog.Warningf("license: refused %s %s: %s", t, addr, msg)
	}
	m.rejectLogAt, m.rejectsSinceLog = now, 0
}

// DisconnectNode ends one stream of a node admitted by AdmitNode. When it
// was the last one, the node's grace period starts. A master that is no
// longer leader leaves its ledger copy alone: the leader owns it.
func (m *Manager) DisconnectNode(t NodeType, addr string) {
	leader := m.raft.IsLeader()
	key := ledgerKey{t, addr}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.conns[key]; n > 1 {
		m.conns[key] = n - 1
		return
	} else if n == 0 {
		return
	}
	delete(m.conns, key)
	if !leader {
		return
	}
	if m.ledger.Disconnect(t, addr, m.now()) {
		m.ledgerDirty = true
	}
}

// ReleaseNode drops the offline ledger entries of addr
// (DELETE /license/nodes/{addr}).
func (m *Manager) ReleaseNode(addr string) error {
	if _, err := m.leaderTerm(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var matched []LedgerEntry
	for _, e := range m.ledger.Entries() {
		if e.Address == addr {
			if e.Online {
				return errorf(CodeNodeOnline, "%s %s is online", e.Type, addr)
			}
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return errorf(CodeNodeNotFound, "%s is not in the ledger", addr)
	}
	for _, e := range matched {
		if err := m.ledger.Release(e.Type, e.Address); err != nil {
			return err
		}
	}
	m.ledgerDirty = true
	return nil
}

// Update returns the LicenseUpdate sent to a node. withKey adds
// internal_read_key: volume servers check the token with it and filers
// present it; other KeepConnected clients never get it (plan 1.4).
func (m *Manager) Update(withKey bool) *Update {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := &Update{
		NewestCertificate: rawOf(m.certs.Newest),
		PrevCertificate:   rawOf(m.certs.Prev),
		MasterNowUnixMs:   m.timeNow().UnixMilli(),
		ClusterID:         m.clusterID,
		MasterDenyReason:  string(m.denyReason),
		StateVersion:      m.version.Load(),
	}
	if withKey {
		u.InternalReadKey = append([]byte(nil), m.key...)
	}
	return u
}

// StateVersion returns LicenseUpdate.state_version.
func (m *Manager) StateVersion() uint64 {
	return m.version.Load()
}

// Deployment returns the license request DTO.
func (m *Manager) Deployment() (DeploymentRequest, error) {
	mem, err := m.raft.Membership()
	if err != nil {
		return DeploymentRequest{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clusterID == "" {
		return DeploymentRequest{}, errorf(CodeReconciling, "cluster_id is not known yet")
	}
	used := m.ledger.Usage()
	used.Masters = mem.size()
	return newDeploymentRequest(m.clusterID, used), nil
}

// NodeCount is one node type in Status.
type NodeCount struct {
	Online int64 `json:"online"`
	Grace  int64 `json:"grace"`
	Limit  int64 `json:"limit"`
}

// GraceNode is a ledger entry in its grace period: it holds a slot until
// ReleaseAt, or until DELETE /license/nodes/{addr} releases it.
type GraceNode struct {
	Type      NodeType `json:"type"`
	Address   string   `json:"address"`
	LastSeen  int64    `json:"last_seen"`
	ReleaseAt int64    `json:"release_at"`
}

// Status is GET /license.
type Status struct {
	Status        Reason               `json:"status"`
	Role          string               `json:"role"`
	Leader        string               `json:"leader"`
	ClusterID     string               `json:"cluster_id"`
	TrustRoot     string               `json:"trust_root"`
	Now           int64                `json:"now"`
	ExpiresAt     int64                `json:"expires_at"`
	DaysRemaining int64                `json:"days_remaining"`
	Effective     *CertificateSummary  `json:"effective"`
	Newest        *CertificateSummary  `json:"newest"`
	Prev          *CertificateSummary  `json:"prev"`
	Nodes         map[string]NodeCount `json:"nodes"`
	GraceNodes    []GraceNode          `json:"grace_nodes"`
	StateVersion  uint64               `json:"state_version"`
	Watermark     int64                `json:"watermark"`
	RepairSeq     int64                `json:"repair_seq"`
	LedgerVersion LedgerVersion        `json:"ledger_version"`
	// VotersAcked lists voters known to hold the current license state;
	// only the leader knows. Check it before shrinking a goraft cluster.
	VotersAcked   []string `json:"voters_acked"`
	VotersPending []string `json:"voters_pending"`
}

// Status describes the license state as seen by this master.
func (m *Manager) Status() *Status {
	now := m.now()
	leader := m.raft.IsLeader()
	reconciling := m.Reconciling()
	leaderID := m.raft.Leader()
	term := m.raft.Term()
	mem, _ := m.raft.Membership()

	m.mu.Lock()
	defer m.mu.Unlock()
	eff := m.certs.Effective(now)
	limits := m.certs.Limits(now)
	s := &Status{
		Role:          "follower",
		Leader:        leaderID,
		ClusterID:     m.clusterID,
		TrustRoot:     "production",
		Now:           now,
		Effective:     eff.Summary(),
		Newest:        m.certs.Newest.Summary(),
		Prev:          m.certs.Prev.Summary(),
		Nodes:         map[string]NodeCount{},
		GraceNodes:    []GraceNode{},
		StateVersion:  m.version.Load(),
		Watermark:     m.clock.Watermark,
		RepairSeq:     m.clock.RepairSeq,
		LedgerVersion: m.ledgerVer,
		VotersAcked:   []string{},
		VotersPending: []string{},
	}
	if m.trust.IsTest() {
		s.TrustRoot = "test"
	}
	switch {
	case reconciling:
		s.Status = ReasonReconciling
	case m.denyReason != "":
		s.Status = m.denyReason
	default:
		s.Status = m.certs.Status(now)
	}
	if eff != nil {
		s.ExpiresAt = eff.Claims.ExpiresAt
		s.DaysRemaining = (eff.Claims.ExpiresAt - now) / 86400
	}
	s.Nodes[string(NodeMaster)] = NodeCount{Online: mem.size(), Limit: limits.MaxMasterNodes}
	for _, t := range []NodeType{NodeVolumeServer, NodeFiler} {
		s.Nodes[string(t)] = NodeCount{Limit: limits.For(t)}
	}
	for _, e := range m.ledger.Entries() {
		c := s.Nodes[string(e.Type)]
		if e.Online {
			c.Online++
		} else {
			c.Grace++
			s.GraceNodes = append(s.GraceNodes, GraceNode{Type: e.Type, Address: e.Address, LastSeen: e.LastSeen, ReleaseAt: e.LastSeen + LedgerGraceSeconds})
		}
		s.Nodes[string(e.Type)] = c
	}
	if leader {
		s.Role = "leader"
		for _, v := range mem.Voters {
			if v == m.self || (m.ackedTerm == term && m.acked[v] == m.rev) {
				s.VotersAcked = append(s.VotersAcked, v)
			} else {
				s.VotersPending = append(s.VotersPending, v)
			}
		}
	}
	return s
}
