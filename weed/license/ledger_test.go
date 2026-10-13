package license

import "testing"

func TestLedgerAdmit(t *testing.T) {
	l := NewLedger(nil)
	limits := Limits{MaxMasterNodes: 3, MaxVolumeServers: 2, MaxFilers: 1}
	for _, addr := range []string{"v1:8080", "v2:8080"} {
		if err := l.Admit(NodeVolumeServer, addr, 100, limits); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Admit(NodeVolumeServer, "v3:8080", 100, limits); codeOf(err) != CodeNodeLimitExceeded {
		t.Fatalf("third volume server: %v", err)
	}
	if err := l.Admit(NodeFiler, "v3:8080", 100, limits); err != nil {
		t.Fatalf("quotas are per node type: %v", err)
	}
	// Known addresses always come back, even over a lowered quota.
	if err := l.Admit(NodeVolumeServer, "v1:8080", 200, Limits{}); err != nil {
		t.Fatalf("re-registration: %v", err)
	}
	if u := l.Usage(); u != (Usage{VolumeServers: 2, Filers: 1}) {
		t.Fatalf("usage %+v", u)
	}
	if err := NewLedger(nil).Admit(NodeVolumeServer, "v1:8080", 100, (Certs{}).Limits(100)); codeOf(err) != CodeNodeLimitExceeded {
		t.Fatalf("an unlicensed cluster must refuse the first volume server: %v", err)
	}
}

func TestLedgerGrace(t *testing.T) {
	l := NewLedger(nil)
	limits := Limits{MaxVolumeServers: 1}
	l.Admit(NodeVolumeServer, "v1", 100, limits)
	l.Disconnect(NodeVolumeServer, "v1", 1000)
	if err := l.Admit(NodeVolumeServer, "v2", 1100, limits); codeOf(err) != CodeNodeLimitExceeded {
		t.Fatalf("a node in grace keeps its slot: %v", err)
	}
	if l.ExpireGrace(1000+LedgerGraceSeconds-1) != 0 {
		t.Fatal("expired before the grace period ended")
	}
	if l.ExpireGrace(1000+LedgerGraceSeconds) != 1 {
		t.Fatal("not expired at the end of the grace period")
	}
	if err := l.Admit(NodeVolumeServer, "v2", 2000, limits); err != nil {
		t.Fatalf("slot freed after grace: %v", err)
	}
}

func TestLedgerRelease(t *testing.T) {
	l := NewLedger(nil)
	l.Admit(NodeFiler, "f1", 1, Limits{MaxFilers: 2})
	if err := l.Release(NodeFiler, "f1"); codeOf(err) != CodeNodeOnline {
		t.Fatalf("online node: %v", err)
	}
	if err := l.Release(NodeFiler, "nope"); codeOf(err) != CodeNodeNotFound {
		t.Fatalf("unknown node: %v", err)
	}
	l.Disconnect(NodeFiler, "f1", 5)
	if err := l.Release(NodeFiler, "f1"); err != nil || l.Usage().Filers != 0 {
		t.Fatalf("release: %v", err)
	}
}

func TestLedgerTakeOver(t *testing.T) {
	l := NewLedger([]LedgerEntry{
		{Address: "v1", Type: NodeVolumeServer, LastSeen: 100, Online: true},
		{Address: "v2", Type: NodeVolumeServer, LastSeen: 50, Online: false},
	})
	l.TakeOver(1000)
	got := l.Entries()
	if got[0] != (LedgerEntry{Address: "v1", Type: NodeVolumeServer, LastSeen: 1000, Online: false}) {
		t.Errorf("online entry must get a full grace period from takeover: %+v", got[0])
	}
	if got[1] != (LedgerEntry{Address: "v2", Type: NodeVolumeServer, LastSeen: 50, Online: false}) {
		t.Errorf("offline entry keeps its deadline: %+v", got[1])
	}
	// A new node cannot take the slot of a node that was online at takeover.
	if err := l.Admit(NodeVolumeServer, "v3", 1001, Limits{MaxVolumeServers: 2}); codeOf(err) != CodeNodeLimitExceeded {
		t.Errorf("v3 must not displace v1: %v", err)
	}
}

func TestLedgerEntriesAndVersion(t *testing.T) {
	l := NewLedger(nil)
	l.Admit(NodeVolumeServer, "b", 1, Limits{MaxVolumeServers: 5, MaxFilers: 5})
	l.Admit(NodeFiler, "z", 1, Limits{MaxVolumeServers: 5, MaxFilers: 5})
	l.Admit(NodeVolumeServer, "a", 1, Limits{MaxVolumeServers: 5, MaxFilers: 5})
	e := l.Entries()
	if len(e) != 3 || e[0].Type != NodeFiler || e[1].Address != "a" || e[2].Address != "b" {
		t.Fatalf("entries not sorted: %+v", e)
	}
	e[0].Online = false
	if !l.Entries()[0].Online {
		t.Fatal("Entries must return a copy")
	}
	if !(LedgerVersion{Term: 1, Revision: 100}).Less(LedgerVersion{Term: 2, Revision: 91}) {
		t.Error("a newer term wins over a higher revision")
	}
	if (LedgerVersion{Term: 2, Revision: 5}).Less(LedgerVersion{Term: 2, Revision: 5}) {
		t.Error("equal versions")
	}
}
