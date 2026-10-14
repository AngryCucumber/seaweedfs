package weed_server

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	hashicorpRaft "github.com/hashicorp/raft"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/cluster"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/topology"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// licenseJoinWait bounds how long a background master join waits for the
// leader to finish its license reconcile.
const licenseJoinWait = 2 * time.Minute

// licenseMetricsInterval is how often the license gauges are refreshed.
const licenseMetricsInterval = 5 * time.Second

var errRaftNotReady = errors.New("raft server is not ready")

// masterRaft is the license manager's view of the master raft group.
type masterRaft struct {
	ms *MasterServer
}

func (r masterRaft) IsLeader() bool { return r.ms.Topo.IsLeader() }

func (r masterRaft) Leader() string {
	t := r.ms.Topo
	t.RaftServerAccessLock.RLock()
	defer t.RaftServerAccessLock.RUnlock()
	switch {
	case t.RaftServer != nil:
		return t.RaftServer.Leader()
	case t.HashicorpRaft != nil:
		_, id := t.HashicorpRaft.LeaderWithID()
		return string(id)
	}
	return ""
}

func (r masterRaft) Term() uint64 {
	t := r.ms.Topo
	t.RaftServerAccessLock.RLock()
	defer t.RaftServerAccessLock.RUnlock()
	switch {
	case t.RaftServer != nil:
		return t.RaftServer.Term()
	case t.HashicorpRaft != nil:
		return t.HashicorpRaft.CurrentTerm()
	}
	return 0
}

func (r masterRaft) Membership() (license.Membership, error) {
	t := r.ms.Topo
	t.RaftServerAccessLock.RLock()
	defer t.RaftServerAccessLock.RUnlock()
	var m license.Membership
	switch {
	case t.RaftServer != nil:
		// goraft has no nonvoters and no configuration index.
		m.Voters = append(m.Voters, t.RaftServer.Name())
		for name := range t.RaftServer.Peers() {
			m.Voters = append(m.Voters, name)
		}
	case t.HashicorpRaft != nil:
		f := t.HashicorpRaft.GetConfiguration()
		if err := f.Error(); err != nil {
			return m, err
		}
		for _, s := range f.Configuration().Servers {
			if s.Suffrage == hashicorpRaft.Voter {
				m.Voters = append(m.Voters, string(s.ID))
			} else {
				m.Nonvoters = append(m.Nonvoters, string(s.ID))
			}
		}
		m.Index = f.Index()
	default:
		return m, errRaftNotReady
	}
	sort.Strings(m.Voters)
	sort.Strings(m.Nonvoters)
	return m, nil
}

func (r masterRaft) hashicorp() (*hashicorpRaft.Raft, error) {
	t := r.ms.Topo
	t.RaftServerAccessLock.RLock()
	defer t.RaftServerAccessLock.RUnlock()
	if t.HashicorpRaft == nil {
		return nil, errors.New("master membership changes need -raftHashicorp")
	}
	return t.HashicorpRaft, nil
}

func (r masterRaft) AddNonvoter(id, address string) error {
	h, err := r.hashicorp()
	if err != nil {
		return err
	}
	return h.AddNonvoter(hashicorpRaft.ServerID(id), hashicorpRaft.ServerAddress(address), 0, 0).Error()
}

func (r masterRaft) AddVoter(id, address string) error {
	h, err := r.hashicorp()
	if err != nil {
		return err
	}
	return h.AddVoter(hashicorpRaft.ServerID(id), hashicorpRaft.ServerAddress(address), 0, 0).Error()
}

func (r masterRaft) RemoveServer(id string) error {
	h, err := r.hashicorp()
	if err != nil {
		return err
	}
	return h.RemoveServer(hashicorpRaft.ServerID(id), 0, 0).Error()
}

// masterPeers sends license RPCs to other masters over gRPC.
type masterPeers struct {
	ms *MasterServer
}

func (p masterPeers) call(peer string, fn func(client master_pb.SeaweedClient) error) error {
	return pb.WithMasterClient(false, pb.ServerAddress(peer), p.ms.grpcDialOption, false, fn)
}

func (p masterPeers) GetLicense(ctx context.Context, peer string) (st license.State, err error) {
	err = p.call(peer, func(client master_pb.SeaweedClient) error {
		resp, err := client.GetLicense(ctx, &master_pb.GetLicenseRequest{IncludeState: true})
		if err == nil {
			st = licenseStateFromPb(resp.State)
		}
		return err
	})
	return
}

func (p masterPeers) GetLedger(ctx context.Context, peer string) (l license.LedgerSnapshot, err error) {
	err = p.call(peer, func(client master_pb.SeaweedClient) error {
		resp, err := client.GetLedger(ctx, &master_pb.GetLedgerRequest{})
		if err == nil {
			l = ledgerFromPb(resp.Ledger)
		}
		return err
	})
	return
}

func (p masterPeers) SyncLicense(ctx context.Context, peer string, from license.LeaderInfo, s license.State) (st license.State, err error) {
	err = p.call(peer, func(client master_pb.SeaweedClient) error {
		resp, err := client.SyncLicense(ctx, &master_pb.SyncLicenseRequest{
			LeaderAddress: from.Address,
			LeaderTerm:    from.Term,
			ConfigIndex:   from.ConfigIndex,
			State:         licenseStateToPb(s),
		})
		if err == nil {
			st = licenseStateFromPb(resp.State)
		}
		return err
	})
	return
}

func (p masterPeers) SyncLedger(ctx context.Context, peer string, from license.LeaderInfo, l license.LedgerSnapshot) error {
	return p.call(peer, func(client master_pb.SeaweedClient) error {
		_, err := client.SyncLedger(ctx, &master_pb.SyncLedgerRequest{
			LeaderAddress: from.Address,
			LeaderTerm:    from.Term,
			Ledger:        ledgerToPb(l),
		})
		return err
	})
}

func licenseStateToPb(s license.State) *master_pb.LicenseState {
	return &master_pb.LicenseState{
		ClusterId:         s.ClusterID,
		NewestCertificate: s.Newest,
		PrevCertificate:   s.Prev,
		InternalReadKey:   s.InternalReadKey,
		Watermark:         s.Clock.Watermark,
		RepairSeq:         s.Clock.RepairSeq,
	}
}

func licenseStateFromPb(s *master_pb.LicenseState) license.State {
	if s == nil {
		return license.State{}
	}
	return license.State{
		ClusterID:       s.ClusterId,
		Newest:          s.NewestCertificate,
		Prev:            s.PrevCertificate,
		InternalReadKey: s.InternalReadKey,
		Clock:           license.ClockState{Watermark: s.Watermark, RepairSeq: s.RepairSeq},
	}
}

func ledgerToPb(l license.LedgerSnapshot) *master_pb.NodeLedgerSnapshot {
	out := &master_pb.NodeLedgerSnapshot{Term: l.Term, Revision: l.Revision}
	for _, e := range l.Entries {
		out.Entries = append(out.Entries, &master_pb.NodeLedgerEntry{
			Address:      e.Address,
			NodeType:     string(e.Type),
			LastSeenUnix: e.LastSeen,
			Online:       e.Online,
		})
	}
	return out
}

func ledgerFromPb(l *master_pb.NodeLedgerSnapshot) license.LedgerSnapshot {
	if l == nil {
		return license.LedgerSnapshot{}
	}
	out := license.LedgerSnapshot{LedgerVersion: license.LedgerVersion{Term: l.Term, Revision: l.Revision}}
	for _, e := range l.Entries {
		out.Entries = append(out.Entries, license.LedgerEntry{
			Address:  e.Address,
			Type:     license.NodeType(e.NodeType),
			LastSeen: e.LastSeenUnix,
			Online:   e.Online,
		})
	}
	return out
}

// licenseUpdateToPb converts the update sent to volume servers and filers.
func licenseUpdateToPb(u *license.Update) *master_pb.LicenseUpdate {
	return &master_pb.LicenseUpdate{
		NewestCertificate: u.NewestCertificate,
		PrevCertificate:   u.PrevCertificate,
		MasterNowUnixMs:   u.MasterNowUnixMs,
		ClusterId:         u.ClusterID,
		MasterDenyReason:  u.MasterDenyReason,
		InternalReadKey:   u.InternalReadKey,
		StateVersion:      u.StateVersion,
	}
}

// licenseGrpcError maps license errors to their gRPC status.
func licenseGrpcError(err error) error {
	var le *license.Error
	if errors.As(err, &le) {
		return status.Error(le.Code.GRPCCode(), le.Error())
	}
	return err
}

// licenseConnected lists the volume servers and filers registered on this
// master, for the reconcile after a leadership change.
func (ms *MasterServer) licenseConnected() (out []license.LedgerEntry) {
	for _, dc := range ms.Topo.Children() {
		for _, rack := range dc.Children() {
			for _, n := range rack.Children() {
				if dn, ok := n.(*topology.DataNode); ok {
					out = append(out, license.LedgerEntry{Address: dn.Url(), Type: license.NodeVolumeServer})
				}
			}
		}
	}
	for _, addr := range ms.Cluster.ListAllClusterNodes(cluster.FilerType) {
		out = append(out, license.LedgerEntry{Address: string(addr), Type: license.NodeFiler})
	}
	return out
}

// licenseJoinMaster adds a master found by OnPeerUpdate or -peers once the
// license reconcile is done; failures only warn.
func (ms *MasterServer) licenseJoinMaster(id, address string) {
	ctx, cancel := context.WithTimeout(context.Background(), licenseJoinWait)
	defer cancel()
	if err := ms.License.WaitReconciled(ctx); err != nil {
		glog.Warningf("license: not adding master %s: %v", id, err)
		return
	}
	if err := ms.License.JoinMaster(ctx, id, address, true); err != nil {
		glog.Warningf("license: adding master %s: %v", id, err)
	}
}

func newLicenseManager(ms *MasterServer) *license.Manager {
	trust, err := license.DefaultTrustStore()
	if err != nil {
		glog.Fatalf("license: %v", err)
	}
	dir := ""
	if !ms.option.IsFollower {
		dir = filepath.Join(util.ResolvePath(ms.option.MetaFolder), license.StateDirName)
	}
	m, err := license.NewManager(license.ManagerOption{
		Dir:         dir,
		Self:        string(ms.option.Master),
		Trust:       trust,
		Raft:        masterRaft{ms},
		Peers:       masterPeers{ms},
		LicenseFile: ms.option.LicenseFile,
		Connected:   ms.licenseConnected,
	})
	if err != nil {
		glog.Fatalf("license: %v", err)
	}
	return m
}

// licenseMetricReasons are the statuses a master reports.
var licenseMetricReasons = []license.Reason{
	license.ReasonValid, license.ReasonNoLicense, license.ReasonNotYetValid, license.ReasonExpired,
	license.ReasonFeatureMissing, license.ReasonClockSuspect, license.ReasonReconciling,
}

// licenseMetricsLoop exports the license status of this master.
func (ms *MasterServer) licenseMetricsLoop() {
	for {
		ms.updateLicenseMetrics()
		time.Sleep(licenseMetricsInterval)
	}
}

func (ms *MasterServer) updateLicenseMetrics() {
	s := ms.License.Status()
	for _, r := range licenseMetricReasons {
		v := 0.0
		if r == s.Status {
			v = 1
		}
		stats.LicenseStateGauge.WithLabelValues(string(r)).Set(v)
	}
	stats.LicenseExpiresAtGauge.Set(float64(s.ExpiresAt))
	for t, c := range s.Nodes {
		stats.LicenseNodesOnlineGauge.WithLabelValues(t).Set(float64(c.Online))
		stats.LicenseNodesGraceGauge.WithLabelValues(t).Set(float64(c.Grace))
		stats.LicenseNodesLimitGauge.WithLabelValues(t).Set(float64(c.Limit))
	}
}
