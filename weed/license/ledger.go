package license

import (
	"sort"
)

// LedgerGraceSeconds is how long a disconnected node keeps its slot.
const LedgerGraceSeconds = 10 * 60

// NodeType is a node kind with its own quota.
type NodeType string

const (
	NodeMaster       NodeType = "master"
	NodeVolumeServer NodeType = "volume_server"
	NodeFiler        NodeType = "filer"
)

// For returns the quota of one node type.
func (l Limits) For(t NodeType) int64 {
	switch t {
	case NodeMaster:
		return l.MaxMasterNodes
	case NodeVolumeServer:
		return l.MaxVolumeServers
	case NodeFiler:
		return l.MaxFilers
	}
	return 0
}

func (u *Usage) add(t NodeType, delta int64) {
	switch t {
	case NodeMaster:
		u.Masters += delta
	case NodeVolumeServer:
		u.VolumeServers += delta
	case NodeFiler:
		u.Filers += delta
	}
}

// For returns the usage of one node type.
func (u Usage) For(t NodeType) int64 {
	return Limits{u.Masters, u.VolumeServers, u.Filers}.For(t)
}

// LedgerEntry is one registered node, online or within its grace period.
type LedgerEntry struct {
	Address  string   `json:"address"`
	Type     NodeType `json:"node_type"`
	LastSeen int64    `json:"last_seen_unix"`
	Online   bool     `json:"online"`
}

// LedgerVersion orders ledger snapshots across leader terms.
type LedgerVersion struct {
	Term     uint64 `json:"term"`
	Revision uint64 `json:"revision"`
}

// Less orders by term, then revision.
func (v LedgerVersion) Less(o LedgerVersion) bool {
	return v.Term < o.Term || (v.Term == o.Term && v.Revision < o.Revision)
}

type ledgerKey struct {
	t    NodeType
	addr string
}

// Ledger is the member ledger: every registered node counts against its
// quota while online and for LedgerGraceSeconds after disconnecting, so a
// node that reconnects within the grace period never loses its slot. It is
// not safe for concurrent use; the master serializes access under the
// license manager lock, which also makes check-and-reserve atomic.
type Ledger struct {
	entries map[ledgerKey]*LedgerEntry
}

// NewLedger returns a ledger holding entries.
func NewLedger(entries []LedgerEntry) *Ledger {
	l := &Ledger{entries: make(map[ledgerKey]*LedgerEntry, len(entries))}
	for i := range entries {
		e := entries[i]
		l.entries[ledgerKey{e.Type, e.Address}] = &e
	}
	return l
}

// Admit registers a node. An address already in the ledger always comes
// back online; a new address needs a free slot under limits.
func (l *Ledger) Admit(t NodeType, addr string, now int64, limits Limits) error {
	key := ledgerKey{t, addr}
	if e, ok := l.entries[key]; ok {
		e.Online, e.LastSeen = true, now
		return nil
	}
	used, limit := l.Usage().For(t), limits.For(t)
	if used >= limit {
		return errorf(CodeNodeLimitExceeded, "%s %d/%d", t, used, limit)
	}
	l.entries[key] = &LedgerEntry{Address: addr, Type: t, LastSeen: now, Online: true}
	return nil
}

// Disconnect starts a node's grace period.
func (l *Ledger) Disconnect(t NodeType, addr string, now int64) {
	if e, ok := l.entries[ledgerKey{t, addr}]; ok {
		e.Online, e.LastSeen = false, now
	}
}

// ExpireGrace drops offline entries whose grace period has ended and
// reports how many were dropped.
func (l *Ledger) ExpireGrace(now int64) int {
	removed := 0
	for k, e := range l.entries {
		if !e.Online && now-e.LastSeen >= LedgerGraceSeconds {
			delete(l.entries, k)
			removed++
		}
	}
	return removed
}

// Release drops an offline entry before its grace period ends. Online
// nodes are never removed.
func (l *Ledger) Release(t NodeType, addr string) error {
	key := ledgerKey{t, addr}
	e, ok := l.entries[key]
	if !ok {
		return errorf(CodeNodeNotFound, "%s %s is not in the ledger", t, addr)
	}
	if e.Online {
		return errorf(CodeNodeOnline, "%s %s is online", t, addr)
	}
	delete(l.entries, key)
	return nil
}

// TakeOver prepares a ledger recovered by a new leader: nodes the old
// leader saw online get a full grace period from now, since their state is
// unknown; offline nodes keep their original deadline.
func (l *Ledger) TakeOver(now int64) {
	for _, e := range l.entries {
		if e.Online {
			e.Online, e.LastSeen = false, now
		}
	}
}

// Usage counts entries per node type.
func (l *Ledger) Usage() Usage {
	var u Usage
	for _, e := range l.entries {
		u.add(e.Type, 1)
	}
	return u
}

// Entries returns a copy of all entries sorted by type and address.
func (l *Ledger) Entries() []LedgerEntry {
	out := make([]LedgerEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Address < out[j].Address
	})
	return out
}
