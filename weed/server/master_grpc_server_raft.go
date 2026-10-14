package weed_server

import (
	"context"
	"fmt"

	"github.com/hashicorp/raft"

	"github.com/seaweedfs/seaweedfs/weed/cluster"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

func (ms *MasterServer) RaftListClusterServers(ctx context.Context, req *master_pb.RaftListClusterServersRequest) (*master_pb.RaftListClusterServersResponse, error) {
	resp := &master_pb.RaftListClusterServersResponse{}

	ms.Topo.RaftServerAccessLock.RLock()
	if ms.Topo.HashicorpRaft == nil {
		ms.Topo.RaftServerAccessLock.RUnlock()
		return resp, nil
	}

	servers := ms.Topo.HashicorpRaft.GetConfiguration().Configuration().Servers
	_, leaderId := ms.Topo.HashicorpRaft.LeaderWithID()
	ms.Topo.RaftServerAccessLock.RUnlock()

	for _, server := range servers {
		resp.ClusterServers = append(resp.ClusterServers, &master_pb.RaftListClusterServersResponse_ClusterServers{
			Id:       string(server.ID),
			Address:  string(server.Address),
			Suffrage: server.Suffrage.String(),
			IsLeader: server.ID == leaderId,
		})
	}
	return resp, nil
}

func (ms *MasterServer) RaftAddServer(ctx context.Context, req *master_pb.RaftAddServerRequest) (*master_pb.RaftAddServerResponse, error) {
	resp := &master_pb.RaftAddServerResponse{}

	ms.Topo.RaftServerAccessLock.RLock()
	hashicorp := ms.Topo.HashicorpRaft
	ms.Topo.RaftServerAccessLock.RUnlock()

	if hashicorp == nil {
		return resp, nil
	}

	if hashicorp.State() != raft.Leader {
		return nil, fmt.Errorf("raft add server %s failed: %s is no current leader", req.Id, hashicorp.String())
	}

	// The license manager checks the master quota and adds a voter as a
	// nonvoter first, promoting it once it holds the license state.
	if err := ms.License.JoinMaster(ctx, req.Id, req.Address, req.Voter); err != nil {
		return nil, licenseGrpcError(err)
	}
	return resp, nil
}

func (ms *MasterServer) RaftRemoveServer(ctx context.Context, req *master_pb.RaftRemoveServerRequest) (*master_pb.RaftRemoveServerResponse, error) {
	resp := &master_pb.RaftRemoveServerResponse{}

	ms.Topo.RaftServerAccessLock.RLock()
	hashicorp := ms.Topo.HashicorpRaft
	ms.Topo.RaftServerAccessLock.RUnlock()

	if hashicorp == nil {
		return resp, nil
	}

	if hashicorp.State() != raft.Leader {
		return nil, fmt.Errorf("raft remove server %s failed: %s is no current leader", req.Id, hashicorp.String())
	}

	if !req.Force {
		ms.clientChansLock.RLock()
		_, ok := ms.clientChans[fmt.Sprintf("%s@%s", cluster.MasterType, req.Id)]
		ms.clientChansLock.RUnlock()
		if ok {
			return resp, fmt.Errorf("raft remove server %s failed: client connection to master exists", req.Id)
		}
	}

	// Removing a voter first recommits the license state to a majority of
	// the voters that remain.
	if err := ms.License.RemoveMaster(ctx, req.Id, func() error {
		return hashicorp.RemoveServer(raft.ServerID(req.Id), 0, 0).Error()
	}); err != nil {
		return nil, licenseGrpcError(err)
	}
	return resp, nil
}
