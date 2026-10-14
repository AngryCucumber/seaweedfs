package license

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCluster is an in-memory raft group of managers wired by direct calls.
type fakeCluster struct {
	t        *testing.T
	trust    *TrustStore
	now      atomic.Int64
	mu       sync.Mutex
	leader   string
	term     uint64
	voters   []string
	nonvoter []string
	index    uint64
	down     map[string]bool
	managers map[string]*Manager
	dirs     map[string]string
	// onSync runs before every SyncLicense delivery.
	onSync func(peer string)
}

func newFakeCluster(t *testing.T, voters ...string) *fakeCluster {
	c := &fakeCluster{t: t, trust: testTrust(t), voters: voters, index: 1,
		down: map[string]bool{}, managers: map[string]*Manager{}, dirs: map[string]string{}}
	c.now.Store(t2026 + 10*86400)
	return c
}

func (c *fakeCluster) clock() time.Time { return time.Unix(c.now.Load(), 0) }

// start creates (or restarts) the manager of id with cluster_id testDeployment.
func (c *fakeCluster) start(id string, edit func(*ManagerOption)) *Manager {
	c.t.Helper()
	mdir, ok := c.dirs[id]
	if !ok {
		mdir = c.t.TempDir()
		if _, err := InitState(mdir, testDeployment); err != nil {
			c.t.Fatal(err)
		}
		c.dirs[id] = mdir
	}
	opt := ManagerOption{
		Dir:   filepath.Join(mdir, StateDirName),
		Self:  id,
		Trust: c.trust,
		Raft:  &fakeRaft{c: c, self: id},
		Peers: &fakePeers{c: c, self: id},
		Now:   c.clock,
	}
	if edit != nil {
		edit(&opt)
	}
	m, err := NewManager(opt)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.managers[id] = m
	c.mu.Unlock()
	return m
}

func (c *fakeCluster) startAll() {
	for _, id := range append(append([]string{}, c.voters...), c.nonvoter...) {
		c.start(id, nil)
	}
}

func (c *fakeCluster) m(id string) *Manager {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.managers[id]
}

// elect makes id leader in a new term and runs its reconcile.
func (c *fakeCluster) elect(id string) uint64 {
	c.mu.Lock()
	c.leader = id
	c.term++
	term := c.term
	c.mu.Unlock()
	c.m(id).reconcile(term)
	return term
}

func (c *fakeCluster) setDown(down bool, ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.down[id] = down
	}
}

type fakeRaft struct {
	c    *fakeCluster
	self string
}

func (r *fakeRaft) IsLeader() bool {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.leader == r.self
}

func (r *fakeRaft) Leader() string {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.leader
}

func (r *fakeRaft) Term() uint64 {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.term
}

func (r *fakeRaft) Membership() (Membership, error) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return Membership{Voters: append([]string{}, r.c.voters...), Nonvoters: append([]string{}, r.c.nonvoter...), Index: r.c.index}, nil
}

func (r *fakeRaft) AddNonvoter(id, address string) error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.c.nonvoter = append(r.c.nonvoter, id)
	r.c.index++
	return nil
}

func (r *fakeRaft) AddVoter(id, address string) error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.c.nonvoter = without(r.c.nonvoter, id)
	r.c.voters = append(r.c.voters, id)
	r.c.index++
	return nil
}

func (r *fakeRaft) RemoveServer(id string) error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.c.voters, r.c.nonvoter = without(r.c.voters, id), without(r.c.nonvoter, id)
	r.c.index++
	return nil
}

type fakePeers struct {
	c    *fakeCluster
	self string
}

func (p *fakePeers) target(peer string) (*Manager, error) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if p.c.down[peer] || p.c.down[p.self] || p.c.managers[peer] == nil {
		return nil, errors.New("unreachable")
	}
	return p.c.managers[peer], nil
}

func (p *fakePeers) GetLicense(ctx context.Context, peer string) (State, error) {
	m, err := p.target(peer)
	if err != nil {
		return State{}, err
	}
	return m.State(), nil
}

func (p *fakePeers) GetLedger(ctx context.Context, peer string) (LedgerSnapshot, error) {
	m, err := p.target(peer)
	if err != nil {
		return LedgerSnapshot{}, err
	}
	return m.Ledger(), nil
}

func (p *fakePeers) SyncLicense(ctx context.Context, peer string, from LeaderInfo, s State) (State, error) {
	if p.c.onSync != nil {
		p.c.onSync(peer)
	}
	m, err := p.target(peer)
	if err != nil {
		return State{}, err
	}
	return m.ApplySync(from, s)
}

func (p *fakePeers) SyncLedger(ctx context.Context, peer string, from LeaderInfo, l LedgerSnapshot) error {
	m, err := p.target(peer)
	if err != nil {
		return err
	}
	return m.ApplyLedgerSync(from, l)
}

func certRaw(t *testing.T, seq int64, edit func(map[string]interface{})) []byte {
	t.Helper()
	return []byte(mustCert(t, func(c map[string]interface{}) {
		c["sequence"] = seq
		if edit != nil {
			edit(c)
		}
	}).Raw())
}

func withLimits(masters, volumes, filers int) func(map[string]interface{}) {
	return func(c map[string]interface{}) {
		c["limits"] = map[string]interface{}{"max_master_nodes": masters, "max_volume_servers": volumes, "max_filers": filers}
	}
}

func newestSeq(m *Manager) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.certs.Newest == nil {
		return 0
	}
	return m.certs.Newest.Claims.Sequence
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	if codeOf(err) != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerSingleMaster(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	c.mu.Lock()
	c.leader, c.term = "a", 1
	c.mu.Unlock()

	if !a.Reconciling() || a.Status().Status != ReasonReconciling {
		t.Fatal("a new leader must reconcile first")
	}
	wantCode(t, a.AdmitNode(NodeVolumeServer, "v1:8080"), CodeReconciling)
	_, err := a.Import(certRaw(t, 1, nil))
	wantCode(t, err, CodeReconciling)

	a.reconcile(1)
	if a.Reconciling() {
		t.Fatal("still reconciling")
	}
	st := a.State()
	if st.ClusterID != testDeployment || len(st.InternalReadKey) != InternalKeySize {
		t.Fatalf("state after reconcile: %+v", st)
	}
	if s := a.Status(); s.Status != ReasonNoLicense || s.Role != "leader" || s.TrustRoot != "test" {
		t.Fatalf("status %+v", s)
	}
	wantCode(t, a.AdmitNode(NodeVolumeServer, "v1:8080"), CodeNodeLimitExceeded)

	v := a.StateVersion()
	out, err := a.Import(certRaw(t, 1, nil))
	if err != nil || !out.Accepted || out.Unchanged || !out.EffectiveNow || len(out.VotersOK) != 1 || out.PeersFailed == nil {
		t.Fatalf("import: %+v %v", out, err)
	}
	if a.StateVersion() == v {
		t.Fatal("state_version must change on import")
	}
	if s := a.Status(); s.Status != ReasonValid || s.Newest.Sequence != 1 || s.Nodes[string(NodeVolumeServer)].Limit != 10 || s.Nodes[string(NodeMaster)].Online != 1 {
		t.Fatalf("status %+v", s)
	}
	if u := a.Update(false); u.NewestCertificate == "" || u.InternalReadKey != nil || u.ClusterID != testDeployment {
		t.Fatalf("update for a shell or broker %+v", u)
	}
	if u := a.Update(true); len(u.InternalReadKey) != InternalKeySize {
		t.Fatal("volume server and filer updates must carry internal_read_key")
	}

	out, err = a.Import(certRaw(t, 1, nil))
	if err != nil || !out.Unchanged || !out.Accepted {
		t.Fatalf("same bytes must be idempotent: %+v %v", out, err)
	}
	_, err = a.Import(certRaw(t, 1, func(c map[string]interface{}) { c["license_id"] = "other" }))
	wantCode(t, err, CodeSequenceConflict)
	_, err = a.Import(certRaw(t, 2, func(c map[string]interface{}) { c["deployment_id"] = "00000000-0000-4000-8000-000000000000" }))
	wantCode(t, err, CodeDeploymentMismatch)
	_, err = a.Import([]byte("not a certificate"))
	wantCode(t, err, CodeMalformed)

	// The state survives a restart.
	b := c.start("a", nil)
	if got := b.State(); got.ClusterID != st.ClusterID || string(got.InternalReadKey) != string(st.InternalReadKey) || newestSeq(b) != 1 {
		t.Fatalf("restarted state %+v", got)
	}
}

func TestManagerImportNeedsVoterMajority(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")

	out, err := a.Import(certRaw(t, 1, nil))
	if err != nil || strings.Join(out.VotersOK, ",") != "a,b,c" {
		t.Fatalf("import: %+v %v", out, err)
	}

	c.setDown(true, "c")
	out, err = a.Import(certRaw(t, 2, nil))
	if err != nil || strings.Join(out.PeersFailed, ",") != "c" || newestSeq(c.m("b")) != 2 {
		t.Fatalf("one follower down: %+v %v", out, err)
	}
	s := a.Status()
	if strings.Join(s.VotersAcked, ",") != "a,b" || strings.Join(s.VotersPending, ",") != "c" {
		t.Fatalf("voters_acked %v pending %v", s.VotersAcked, s.VotersPending)
	}

	c.setDown(true, "b")
	out, err = a.Import(certRaw(t, 3, nil))
	wantCode(t, err, CodePartialCommit)
	if out == nil || out.Accepted || strings.Join(out.VotersOK, ",") != "a" || newestSeq(a) != 3 {
		t.Fatalf("partial commit: %+v", out)
	}

	c.setDown(false, "b", "c")
	out, err = a.Import(certRaw(t, 3, nil))
	if err != nil || !out.Unchanged || strings.Join(out.VotersOK, ",") != "a,b,c" || newestSeq(c.m("c")) != 3 {
		t.Fatalf("retry: %+v %v", out, err)
	}
}

func TestManagerNonvoterDoesNotCount(t *testing.T) {
	c := newFakeCluster(t, "a", "b")
	c.nonvoter = []string{"n"}
	c.startAll()
	c.elect("a")
	c.setDown(true, "b")
	out, err := c.m("a").Import(certRaw(t, 1, nil))
	wantCode(t, err, CodePartialCommit)
	if strings.Join(out.VotersOK, ",") != "a" || newestSeq(c.m("n")) != 1 {
		t.Fatalf("outcome %+v", out)
	}
}

func TestManagerRejectsStaleSender(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	term := c.elect("a")
	st := c.m("a").State()
	b := c.m("b")
	_, err := b.ApplySync(LeaderInfo{Address: "a", Term: term - 1}, st)
	wantCode(t, err, CodeNotLeader)
	_, err = b.ApplySync(LeaderInfo{Address: "c", Term: term}, st)
	wantCode(t, err, CodeNotLeader)
	wantCode(t, b.ApplyLedgerSync(LeaderInfo{Address: "a", Term: term - 1}, LedgerSnapshot{}), CodeNotLeader)
	if _, err := b.ApplySync(LeaderInfo{Address: "a", Term: term}, st); err != nil {
		t.Fatal(err)
	}
}

func TestManagerReconcileWritesBack(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c", "d", "e")
	c.startAll()
	c.elect("a")
	if _, err := c.m("a").Import(certRaw(t, 10, withLimits(5, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// 11 reaches only A.
	c.setDown(true, "b", "c", "d", "e")
	_, err := c.m("a").Import(certRaw(t, 11, withLimits(5, 1, 1)))
	wantCode(t, err, CodePartialCommit)

	// B wins with only A and itself reachable: no majority, keeps reconciling.
	c.setDown(false, "b")
	c.elect("b")
	if !c.m("b").Reconciling() {
		t.Fatal("reconcile must need a voter majority")
	}
	wantCode(t, c.m("b").AdmitNode(NodeFiler, "f:8888"), CodeReconciling)

	// With C reachable B picks 11 and writes it back before it opens.
	c.setDown(false, "c")
	c.elect("b")
	if c.m("b").Reconciling() || newestSeq(c.m("b")) != 11 || newestSeq(c.m("c")) != 11 {
		t.Fatalf("reconcile: b=%d c=%d", newestSeq(c.m("b")), newestSeq(c.m("c")))
	}

	// A and B are gone; C, D and E still recover 11.
	c.setDown(true, "a", "b")
	c.setDown(false, "d", "e")
	c.elect("c")
	if c.m("c").Reconciling() || newestSeq(c.m("d")) != 11 || newestSeq(c.m("e")) != 11 {
		t.Fatalf("after failover: d=%d e=%d", newestSeq(c.m("d")), newestSeq(c.m("e")))
	}
	if c.m("c").State().ClusterID != testDeployment {
		t.Fatal("cluster_id changed")
	}
}

func TestManagerReconcileGeneratesClusterID(t *testing.T) {
	c := newFakeCluster(t, "a")
	mdir := t.TempDir()
	c.dirs["a"] = mdir // no license.init
	a := c.start("a", nil)
	c.elect("a")
	st := a.State()
	if !canonicalUUID.MatchString(st.ClusterID) || a.Reconciling() {
		t.Fatalf("cluster_id %q", st.ClusterID)
	}
	d, err := a.Deployment()
	if err != nil || d.DeploymentID != st.ClusterID || d.Product != Product || d.RegisteredMasterNodes != 1 {
		t.Fatalf("deployment %+v %v", d, err)
	}
	// license.init now refuses another id for this mdir.
	if _, err := InitState(mdir, testDeployment); err == nil {
		t.Fatal("license.init must not replace a cluster_id")
	}
}

func TestManagerLedgerVersionOrder(t *testing.T) {
	c := newFakeCluster(t, "a", "b")
	c.startAll()
	b := c.m("b")
	c.mu.Lock()
	c.leader = "a"
	c.mu.Unlock()
	sync := func(term, rev uint64, addrs ...string) error {
		c.mu.Lock()
		c.term = term
		c.mu.Unlock()
		l := LedgerSnapshot{LedgerVersion: LedgerVersion{Term: term, Revision: rev}}
		for _, a := range addrs {
			l.Entries = append(l.Entries, LedgerEntry{Address: a, Type: NodeVolumeServer, LastSeen: 1, Online: true})
		}
		return b.ApplyLedgerSync(LeaderInfo{Address: "a", Term: term}, l)
	}
	if err := sync(1, 100, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := sync(2, 91, "v2"); err != nil {
		t.Fatal("a higher term wins over a higher revision:", err)
	}
	if err := sync(2, 91, "v2"); err != nil {
		t.Fatal("same version and content is fine:", err)
	}
	if err := sync(2, 91, "v3"); err == nil {
		t.Fatal("same version with other entries must be refused")
	}
	c.mu.Lock()
	c.term = 2
	c.mu.Unlock()
	if err := b.ApplyLedgerSync(LeaderInfo{Address: "a", Term: 2}, LedgerSnapshot{LedgerVersion: LedgerVersion{Term: 1, Revision: 101}}); err == nil {
		t.Fatal("an older version must be refused")
	}
	if l := b.Ledger(); l.Term != 2 || l.Revision != 91 || len(l.Entries) != 1 || l.Entries[0].Address != "v2" {
		t.Fatalf("ledger %+v", l)
	}
}

func TestManagerLedgerReplicationAndTakeOver(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 1, withLimits(3, 2, 1))); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1:8080", "v2:8080"} {
		if err := a.AdmitNode(NodeVolumeServer, v); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, a.AdmitNode(NodeVolumeServer, "v3:8080"), CodeNodeLimitExceeded)
	a.DisconnectNode(NodeVolumeServer, "v2:8080")

	term := c.term
	a.replicateLedger(term)
	waitFor(t, "ledger batch", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.ledgerSending
	})
	if l := c.m("b").Ledger(); len(l.Entries) != 2 || l.Term != term {
		t.Fatalf("follower ledger %+v", l)
	}
	// A new batch waits for retryInterval.
	a.mu.Lock()
	a.ledgerDirty = true
	a.mu.Unlock()
	a.replicateLedger(term)
	a.mu.Lock()
	sending := a.ledgerSending
	a.mu.Unlock()
	if sending {
		t.Fatal("batches must be at least retryInterval apart")
	}

	// B takes over: v1 was online, so it gets a full grace period from
	// now; v2 keeps its deadline. v1 is still connected to nobody.
	c.now.Add(60)
	c.start("b", func(o *ManagerOption) {
		o.Connected = func() []LedgerEntry { return nil }
	})
	newTerm := c.elect("b")
	l := c.m("b").Ledger()
	if l.Term != newTerm || l.Revision != 1 {
		t.Fatalf("version %+v", l.LedgerVersion)
	}
	for _, e := range l.Entries {
		if e.Online {
			t.Fatalf("entry %+v must be offline after takeover", e)
		}
		if e.Address == "v1:8080" && e.LastSeen != c.now.Load() {
			t.Fatalf("v1 last_seen %d", e.LastSeen)
		}
		if e.Address == "v2:8080" && e.LastSeen != c.now.Load()-60 {
			t.Fatalf("v2 keeps last_seen, got %d", e.LastSeen)
		}
	}
	if got := c.m("c").Ledger(); got.LedgerVersion != l.LedgerVersion {
		t.Fatalf("write-back %+v", got.LedgerVersion)
	}
	// Both entries still hold their slots.
	wantCode(t, c.m("b").AdmitNode(NodeVolumeServer, "v3:8080"), CodeNodeLimitExceeded)
	if err := c.m("b").AdmitNode(NodeVolumeServer, "v2:8080"); err != nil {
		t.Fatal("a ledger address re-registers:", err)
	}

	// A leader that keeps streams across terms keeps them online.
	c.start("c", func(o *ManagerOption) {
		o.Connected = func() []LedgerEntry { return []LedgerEntry{{Address: "v2:8080", Type: NodeVolumeServer}} }
	})
	c.elect("c")
	for _, e := range c.m("c").Ledger().Entries {
		if e.Online != (e.Address == "v2:8080") {
			t.Fatalf("entry %+v", e)
		}
	}
}

func TestManagerLedgerSurvivesTermReset(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	first := c.elect("a")
	if _, err := c.m("a").Import(certRaw(t, 1, withLimits(3, 2, 1))); err != nil {
		t.Fatal(err)
	}
	if err := c.m("a").AdmitNode(NodeVolumeServer, "v1:8080"); err != nil {
		t.Fatal(err)
	}
	batchDone := func(id string) func() bool {
		return func() bool {
			m := c.m(id)
			m.mu.Lock()
			defer m.mu.Unlock()
			return !m.ledgerSending && !m.ledgerPending
		}
	}
	c.m("a").replicateLedger(first)
	waitFor(t, "ledger batch", batchDone("a"))
	c.elect("b")
	before := c.m("b").Ledger().LedgerVersion

	// goraft deletes its log on start: after all masters restart, terms
	// count from the beginning again while state.json keeps the ledger.
	c.mu.Lock()
	c.term = 0
	c.mu.Unlock()
	c.startAll()
	term := c.elect("c")
	if term > before.Term {
		t.Fatalf("test needs a raft term below the ledger term %d, got %d", before.Term, term)
	}
	if _, err := c.m("c").leaderTerm(); err != nil {
		t.Fatal("reconcile after a term reset:", err)
	}
	l := c.m("c").Ledger()
	if !before.Less(l.LedgerVersion) || len(l.Entries) != 1 || l.Entries[0].Address != "v1:8080" {
		t.Fatalf("ledger %+v after %+v", l, before)
	}
	for _, id := range []string{"a", "b"} {
		if got := c.m(id).Ledger().LedgerVersion; got != l.LedgerVersion {
			t.Fatalf("%s holds %+v, want %+v", id, got, l.LedgerVersion)
		}
	}

	// Batches in this term keep the epoch and move the revision.
	c.m("c").AdmitNode(NodeVolumeServer, "v2:8080")
	c.now.Add(int64(retryInterval / time.Second))
	c.m("c").replicateLedger(term)
	waitFor(t, "ledger batch", batchDone("c"))
	next := c.m("a").Ledger().LedgerVersion
	if next.Term != l.Term || next.Revision != l.Revision+1 {
		t.Fatalf("batch version %+v after %+v", next, l.LedgerVersion)
	}

	// The next leader, still below the old terms, writes above it again.
	c.elect("a")
	if got := c.m("a").Ledger().LedgerVersion; !next.Less(got) || c.m("b").Ledger().LedgerVersion != got {
		t.Fatalf("next leader %+v after %+v", got, next)
	}
}

func TestManagerAcksOnlyDurableState(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 1, nil)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	failing := map[string]bool{}
	setFailing := func(id string, fail bool) {
		mu.Lock()
		defer mu.Unlock()
		failing[filepath.Join(c.dirs[id], StateDirName, StateFileName)] = fail
	}
	saved := writeState
	writeState = func(path string, s *stateFile) error {
		mu.Lock()
		fail := failing[path]
		mu.Unlock()
		if fail {
			return errors.New("injected write failure")
		}
		return saved(path, s)
	}
	defer func() { writeState = saved }()
	has := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}

	// A follower whose disk fails is not counted, also when the same
	// certificate is pushed again and its memory already holds it.
	setFailing("b", true)
	cert2 := certRaw(t, 2, nil)
	for i := 0; i < 2; i++ {
		out, err := a.Import(cert2)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if has(out.VotersOK, "b") || !has(out.PeersFailed, "b") {
			t.Fatalf("attempt %d: b counted without a durable write: %+v", i, out)
		}
	}
	setFailing("b", false)
	if out, err := a.Import(cert2); err != nil || !has(out.VotersOK, "b") {
		t.Fatalf("after b recovers: %+v, %v", out, err)
	}
	c.start("b", nil)
	if seq := newestSeq(c.m("b")); seq != 2 {
		t.Fatalf("b restarted with sequence %d", seq)
	}

	// A leader whose disk fails refuses the import, also on a retry.
	setFailing("a", true)
	cert3 := certRaw(t, 3, nil)
	for i := 0; i < 2; i++ {
		if _, err := a.Import(cert3); err == nil {
			t.Fatalf("attempt %d: import succeeded without a durable write", i)
		}
	}
	setFailing("a", false)
	if _, err := a.Import(cert3); err != nil {
		t.Fatal(err)
	}
	c.start("a", nil)
	if seq := newestSeq(c.m("a")); seq != 3 {
		t.Fatalf("a restarted with sequence %d", seq)
	}
}

func TestManagerPrevConverges(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	term := c.elect("a")
	a := c.m("a")
	now := c.now.Load()
	cert := func(seq, notBefore, expiresAt int64) []byte {
		return certRaw(t, seq, func(m map[string]interface{}) {
			m["not_before"], m["expires_at"], m["issued_at"] = notBefore, expiresAt, min(notBefore, now)
		})
	}
	c2 := cert(2, t2026, now+100) // expires soon
	c3 := cert(3, t2026, t2027)
	c4 := cert(4, now+1000, t2027) // starts later
	if _, err := a.Import(c2); err != nil {
		t.Fatal(err)
	}

	// b misses 3 and 4, then starts with -license.file 4 on top of 2:
	// it holds (4, prev 2) while a and c hold (4, prev 3).
	c.setDown(true, "b")
	for _, raw := range [][]byte{c3, c4} {
		if _, err := a.Import(raw); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "cert.jws")
	if err := os.WriteFile(file, c4, 0600); err != nil {
		t.Fatal(err)
	}
	c.start("b", func(o *ManagerOption) { o.LicenseFile = file })
	prevSeq := func(id string) int64 {
		m := c.m(id)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.certs.Prev == nil {
			return 0
		}
		return m.certs.Prev.Claims.Sequence
	}
	if prevSeq("b") != 2 {
		t.Fatalf("setup: b prev %d", prevSeq("b"))
	}
	c.setDown(false, "b")

	// The leader's push brings b the better Prev, and b counts only then.
	c.now.Add(int64(retryInterval / time.Second))
	a.pushPending(term)
	waitFor(t, "b confirms the leader's Prev", func() bool {
		a.mu.Lock()
		acked := a.acked["b"] == a.rev
		a.mu.Unlock()
		return acked && prevSeq("b") == 3
	})
	c.now.Add(200) // 2 has expired, 4 has not started
	if s := c.m("b").Status(); s.Status != ReasonValid {
		t.Fatalf("b status %s", s.Status)
	}

	// A leader holding the worse Prev takes the better one at reconcile.
	worse, err := ParseCertificate(c2, c.trust)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.certs.Prev = worse
	a.mu.Unlock()
	c.elect("a")
	if prevSeq("a") != 3 || a.Status().Status != ReasonValid {
		t.Fatalf("a prev %d, status %s after reconcile", prevSeq("a"), a.Status().Status)
	}
}

func TestManagerAdmissionAndRelease(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	c.elect("a")
	err := a.AdmitNode(NodeFiler, "f1:8888")
	wantCode(t, err, CodeNodeLimitExceeded)
	if !strings.Contains(err.Error(), "no license imported") {
		t.Fatalf("message %q", err)
	}
	if _, err := a.Import(certRaw(t, 1, withLimits(1, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := a.AdmitNode(NodeFiler, "f1:8888"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.AdmitNode(NodeFiler, "f2:8888"), CodeNodeLimitExceeded)
	if err := a.AdmitNode(NodeVolumeServer, "v1:8080"); err != nil {
		t.Fatal("quotas are per type:", err)
	}
	wantCode(t, a.ReleaseNode("f1:8888"), CodeNodeOnline)
	wantCode(t, a.ReleaseNode("x:1"), CodeNodeNotFound)
	a.DisconnectNode(NodeFiler, "f1:8888")
	s := a.Status()
	if s.Nodes[string(NodeFiler)] != (NodeCount{Online: 0, Grace: 1, Limit: 1}) {
		t.Fatalf("nodes %+v", s.Nodes)
	}
	now := c.now.Load()
	if want := []GraceNode{{Type: NodeFiler, Address: "f1:8888", LastSeen: now, ReleaseAt: now + LedgerGraceSeconds}}; !reflect.DeepEqual(s.GraceNodes, want) {
		t.Fatalf("grace nodes %+v, want %+v", s.GraceNodes, want)
	}
	wantCode(t, a.AdmitNode(NodeFiler, "f2:8888"), CodeNodeLimitExceeded)
	if err := a.ReleaseNode("f1:8888"); err != nil {
		t.Fatal(err)
	}
	if g := a.Status().GraceNodes; len(g) != 0 {
		t.Fatalf("released node still listed: %+v", g)
	}
	if err := a.AdmitNode(NodeFiler, "f2:8888"); err != nil {
		t.Fatal(err)
	}
	// Grace expiry frees a slot too.
	a.DisconnectNode(NodeFiler, "f2:8888")
	c.now.Add(LedgerGraceSeconds)
	a.tick()
	waitFor(t, "ledger batch", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.ledgerSending
	})
	if err := a.AdmitNode(NodeFiler, "f3:8888"); err != nil {
		t.Fatal(err)
	}
	// A renewal with the same quotas still fits the ledger.
	_, err = a.Import(certRaw(t, 2, withLimits(1, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AdmitNode(NodeVolumeServer, "v2:8080"); err == nil {
		t.Fatal("over quota")
	}
}

func TestManagerNodeStreams(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 1, withLimits(3, 1, 1))); err != nil {
		t.Fatal(err)
	}
	entry := func(m *Manager, addr string) LedgerEntry {
		for _, e := range m.Ledger().Entries {
			if e.Address == addr {
				return e
			}
		}
		t.Fatalf("%s is not in the ledger", addr)
		return LedgerEntry{}
	}

	// A reconnect whose old stream closes late keeps the node online.
	for i := 0; i < 2; i++ {
		if err := a.AdmitNode(NodeVolumeServer, "v1:8080"); err != nil {
			t.Fatal(err)
		}
	}
	a.DisconnectNode(NodeVolumeServer, "v1:8080")
	if e := entry(a, "v1:8080"); !e.Online {
		t.Fatalf("one of two streams closed: %+v", e)
	}
	a.DisconnectNode(NodeVolumeServer, "v1:8080")
	grace := entry(a, "v1:8080")
	if grace.Online || grace.LastSeen != c.now.Load() {
		t.Fatalf("last stream closed: %+v", grace)
	}
	// A stray disconnect does not move the grace deadline.
	c.now.Add(30)
	a.DisconnectNode(NodeVolumeServer, "v1:8080")
	if e := entry(a, "v1:8080"); e != grace {
		t.Fatalf("grace moved: %+v", e)
	}
	err := a.AdmitNode(NodeVolumeServer, "v2:8080")
	if err == nil || err.Error() != "license: NODE_LIMIT_EXCEEDED volume_server 1/1" {
		t.Fatalf("message %v", err)
	}

	// A stream that ends after this master lost leadership leaves the
	// ledger to the new leader.
	if err := a.AdmitNode(NodeFiler, "f1:8888"); err != nil {
		t.Fatal(err)
	}
	c.setDown(true, "a")
	c.elect("b")
	c.setDown(false, "a")
	a.DisconnectNode(NodeFiler, "f1:8888")
	if e := entry(a, "f1:8888"); !e.Online {
		t.Fatalf("a follower changed its ledger copy: %+v", e)
	}
	a.mu.Lock()
	streams := len(a.conns)
	a.mu.Unlock()
	if streams != 0 {
		t.Fatalf("%d streams left", streams)
	}
}

func TestManagerImportChecksLedgerAndMasters(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 1, nil)); err != nil {
		t.Fatal(err)
	}
	if err := a.AdmitNode(NodeVolumeServer, "v1:8080"); err != nil {
		t.Fatal(err)
	}
	if err := a.AdmitNode(NodeVolumeServer, "v2:8080"); err != nil {
		t.Fatal(err)
	}
	_, err := a.Import(certRaw(t, 2, withLimits(3, 1, 2)))
	wantCode(t, err, CodeNodeLimitTooSmall)
	_, err = a.Import(certRaw(t, 2, withLimits(2, 10, 2)))
	wantCode(t, err, CodeNodeLimitTooSmall)
	if newestSeq(a) != 1 {
		t.Fatal("a failed import must keep the old state")
	}
}

func TestManagerJoinMaster(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	c.start("d", nil)
	c.elect("a")
	ctx := context.Background()

	err := a.JoinMaster(ctx, "d", "d:19333", true)
	wantCode(t, err, CodeNodeLimitExceeded)
	if !strings.Contains(err.Error(), "no license imported") {
		t.Fatalf("message %q", err)
	}

	if _, err := a.Import(certRaw(t, 1, withLimits(2, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := a.JoinMaster(ctx, "d", "d:19333", true); err != nil {
		t.Fatal(err)
	}
	mem, _ := a.raft.Membership()
	if strings.Join(mem.Voters, ",") != "a,d" || len(mem.Nonvoters) != 0 || newestSeq(c.m("d")) != 1 {
		t.Fatalf("membership %+v", mem)
	}
	if err := a.JoinMaster(ctx, "d", "d:19333", true); err != nil {
		t.Fatal("joining twice is a no-op:", err)
	}
	c.start("e", nil)
	wantCode(t, a.JoinMaster(ctx, "e", "e:19333", false), CodeNodeLimitExceeded)
}

func TestManagerJoinKeepsLedgerPending(t *testing.T) {
	c := newFakeCluster(t, "a", "b")
	c.startAll()
	c.start("d", nil)
	term := c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 1, nil)); err != nil {
		t.Fatal(err)
	}
	if err := a.AdmitNode(NodeVolumeServer, "v1:8080"); err != nil {
		t.Fatal(err)
	}
	// The join pushes the unsent change to D under a new version ...
	if err := a.JoinMaster(context.Background(), "d", "d:19333", true); err != nil {
		t.Fatal(err)
	}
	if l := c.m("d").Ledger(); len(l.Entries) != 1 {
		t.Fatalf("joiner ledger %+v", l)
	}
	// ... and the next batch still sends it to everyone else.
	a.replicateLedger(term)
	waitFor(t, "ledger batch", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.ledgerSending
	})
	if l := c.m("b").Ledger(); len(l.Entries) != 1 || l.LedgerVersion != a.Ledger().LedgerVersion {
		t.Fatalf("follower ledger %+v, leader %+v", l, a.Ledger().LedgerVersion)
	}
	a.mu.Lock()
	pending := a.ledgerPending
	a.mu.Unlock()
	if pending {
		t.Fatal("a confirmed batch clears pending")
	}
}

func TestManagerJoinMasterStaysNonvoter(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	c.start("d", nil)
	c.elect("a")
	if _, err := a.Import(certRaw(t, 1, nil)); err != nil {
		t.Fatal(err)
	}
	c.setDown(true, "d")
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := a.JoinMaster(ctx, "d", "d:19333", true); err == nil {
		t.Fatal("an unconfirmed master must not be promoted")
	}
	mem, _ := a.raft.Membership()
	if strings.Join(mem.Voters, ",") != "a" || strings.Join(mem.Nonvoters, ",") != "d" {
		t.Fatalf("membership %+v", mem)
	}
}

func TestManagerRemoveMasterRecommits(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 10, nil)); err != nil {
		t.Fatal(err)
	}
	c.setDown(true, "c")
	if _, err := a.Import(certRaw(t, 11, nil)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	removed := 0
	remove := func(id string) func() error {
		return func() error { removed++; return a.raft.RemoveServer(id) }
	}

	// C is unreachable: removing B would leave 11 on A only.
	err := a.RemoveMaster(ctx, "b", remove("b"))
	wantCode(t, err, CodePartialCommit)
	if removed != 0 {
		t.Fatal("removal must wait for the recommit")
	}

	// The membership changes during the recommit.
	c.setDown(false, "c")
	c.onSync = func(string) {
		c.mu.Lock()
		c.index++
		c.mu.Unlock()
	}
	wantCode(t, a.RemoveMaster(ctx, "b", remove("b")), CodeMembershipChanged)
	c.onSync = nil

	if err := a.RemoveMaster(ctx, "b", remove("b")); err != nil {
		t.Fatal(err)
	}
	if removed != 1 || newestSeq(c.m("c")) != 11 {
		t.Fatalf("removed=%d c=%d", removed, newestSeq(c.m("c")))
	}
}

func TestManagerClockSuspectAndRepair(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a, b := c.m("a"), c.m("b")
	if _, err := a.Import(certRaw(t, 1, nil)); err != nil {
		t.Fatal(err)
	}
	now := c.now.Load()
	a.refreshClock(now)
	v := a.StateVersion()

	c.now.Store(now - 10)
	a.refreshClock(c.now.Load())
	if s := a.Status(); s.Status != ReasonClockSuspect {
		t.Fatalf("status %s", s.Status)
	}
	if u := a.Update(false); u.MasterDenyReason != string(ReasonClockSuspect) || u.StateVersion == v {
		t.Fatalf("update %+v", u)
	}

	// B saw a future time; same-repair syncs keep the higher watermark.
	b.mu.Lock()
	b.clock = ClockState{Watermark: now + 1000, RepairSeq: 0}
	b.mu.Unlock()

	// A higher sequence issued before now repairs the clock everywhere.
	issued := c.now.Load() - 100
	out, err := a.Import(certRaw(t, 2, func(m map[string]interface{}) { m["issued_at"] = issued }))
	if err != nil || len(out.VotersOK) != 3 {
		t.Fatalf("repair import %+v %v", out, err)
	}
	a.refreshClock(c.now.Load())
	if s := a.Status(); s.Status != ReasonValid || s.RepairSeq != 2 {
		t.Fatalf("status %+v", s)
	}
	if st := b.State(); st.Clock.RepairSeq != 2 || st.Clock.Watermark != issued {
		t.Fatalf("follower clock %+v", st.Clock)
	}

	// A late request of the old repair does not raise it again.
	late := a.State()
	late.Clock = ClockState{Watermark: now + 5000, RepairSeq: 1}
	if _, err := b.ApplySync(LeaderInfo{Address: "a", Term: c.term}, late); err != nil {
		t.Fatal(err)
	}
	if st := b.State(); st.Clock.Watermark != issued {
		t.Fatalf("late repair raised the watermark to %d", st.Clock.Watermark)
	}
	// Re-importing the same certificate does not reset the watermark.
	a.refreshClock(c.now.Load())
	before := a.State().Clock
	if _, err := a.Import(certRaw(t, 2, func(m map[string]interface{}) { m["issued_at"] = issued })); err != nil {
		t.Fatal(err)
	}
	if a.State().Clock != before {
		t.Fatal("idempotent import changed the clock state")
	}
}

func TestManagerAdoptsNewerFollowerState(t *testing.T) {
	c := newFakeCluster(t, "a", "b", "c")
	c.startAll()
	c.elect("a")
	a := c.m("a")
	if _, err := a.Import(certRaw(t, 3, nil)); err != nil {
		t.Fatal(err)
	}
	five := mustCert(t, func(m map[string]interface{}) { m["sequence"] = 5 })
	cm := c.m("c")
	cm.mu.Lock()
	cm.certs = Certs{Newest: five}
	cm.mu.Unlock()
	out, err := a.Import(certRaw(t, 4, nil))
	if err != nil || strings.Join(out.PeersFailed, ",") != "c" {
		t.Fatalf("import %+v %v", out, err)
	}
	if newestSeq(a) != 5 {
		t.Fatalf("leader did not adopt sequence 5: %d", newestSeq(a))
	}
}

func TestManagerCheckBootstrap(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	if err := a.CheckBootstrap(1); err != nil {
		t.Fatal(err)
	}
	err := a.CheckBootstrap(3)
	wantCode(t, err, CodeNodeLimitExceeded)
	if !strings.Contains(err.Error(), "master 3/0") || !strings.Contains(err.Error(), "license.init") {
		t.Fatalf("message %q", err)
	}
	c.elect("a")
	if _, err := a.Import(certRaw(t, 1, withLimits(3, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckBootstrap(3); err != nil {
		t.Fatal(err)
	}
	wantCode(t, a.CheckBootstrap(5), CodeNodeLimitExceeded)
}

func TestManagerLicenseFile(t *testing.T) {
	c := newFakeCluster(t, "a")
	file := filepath.Join(t.TempDir(), "license.jws")
	write := func(raw []byte) {
		if err := os.WriteFile(file, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	withFile := func(o *ManagerOption) { o.LicenseFile = file }
	write(certRaw(t, 1, withLimits(3, 1, 1)))

	// Without license.init there is no cluster_id to check against.
	c.dirs["a"] = t.TempDir()
	_, err := NewManager(ManagerOption{Dir: filepath.Join(c.dirs["a"], StateDirName), Self: "a", Trust: c.trust,
		Raft: &fakeRaft{c: c, self: "a"}, Peers: &fakePeers{c: c, self: "a"}, Now: c.clock, LicenseFile: file})
	if err == nil || !strings.Contains(err.Error(), "license.init") {
		t.Fatalf("got %v", err)
	}
	delete(c.dirs, "a")

	a := c.start("a", withFile)
	if newestSeq(a) != 1 || a.CheckBootstrap(3) != nil {
		t.Fatal("-license.file must apply before raft starts")
	}
	a = c.start("a", withFile)
	if newestSeq(a) != 1 {
		t.Fatal("restart with the same file")
	}

	write(certRaw(t, 2, func(m map[string]interface{}) { m["deployment_id"] = "00000000-0000-4000-8000-000000000000" }))
	if _, err := NewManager(ManagerOption{Dir: filepath.Join(c.dirs["a"], StateDirName), Self: "a", Trust: c.trust,
		Raft: &fakeRaft{c: c, self: "a"}, Now: c.clock, LicenseFile: file}); err == nil || !strings.Contains(err.Error(), string(CodeDeploymentMismatch)) {
		t.Fatalf("got %v", err)
	}
	write(certRaw(t, 2, func(m map[string]interface{}) { m["expires_at"] = c.now.Load() }))
	if _, err := NewManager(ManagerOption{Dir: filepath.Join(c.dirs["a"], StateDirName), Self: "a", Trust: c.trust,
		Raft: &fakeRaft{c: c, self: "a"}, Now: c.clock, LicenseFile: file}); err == nil || !strings.Contains(err.Error(), string(CodeExpired)) {
		t.Fatalf("got %v", err)
	}
}

func TestInitState(t *testing.T) {
	mdir := t.TempDir()
	d, err := InitState(mdir, "")
	if err != nil || !canonicalUUID.MatchString(d.DeploymentID) || d.RegisteredMasterNodes != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	again, err := InitState(mdir, "")
	if err != nil || again.DeploymentID != d.DeploymentID {
		t.Fatal("license.init must be idempotent")
	}
	if _, err := InitState(mdir, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := InitState(mdir, testDeployment); err == nil {
		t.Fatal("must refuse another cluster_id")
	}
	if _, err := InitState(t.TempDir(), "C9B82051-A34E-455B-BFF3-2F09306C6AA8"); err == nil {
		t.Fatal("must refuse a non-canonical id")
	}
	info, err := os.Stat(filepath.Join(mdir, StateDirName, StateFileName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file %v %v", info, err)
	}
}

func TestManagerValidateRateLimit(t *testing.T) {
	c := newFakeCluster(t, "a")
	a := c.start("a", nil)
	raw := certRaw(t, 1, nil)
	for i := 0; i < validatePerMinute; i++ {
		out, err := a.Validate(raw)
		if err != nil || !out.Valid || !out.EffectiveNow {
			t.Fatalf("validate %d: %+v %v", i, out, err)
		}
	}
	_, err := a.Validate(raw)
	wantCode(t, err, CodeRateLimited)
	c.now.Add(6)
	if _, err := a.Validate(raw); err != nil {
		t.Fatal(err)
	}
	if newestSeq(a) != 0 {
		t.Fatal("validate must not import")
	}
}
