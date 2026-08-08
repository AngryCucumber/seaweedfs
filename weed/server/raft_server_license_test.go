package weed_server

import (
	"encoding/json"
	"strings"
	"testing"

	hashicorpRaft "github.com/hashicorp/raft"
	"github.com/seaweedfs/seaweedfs/weed/massdblicense"
	"github.com/seaweedfs/seaweedfs/weed/sequence"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/topology"
)

func TestRaftSnapshotPersistsMassDBLicenseState(t *testing.T) {
	topo := topology.NewTopology("source", sequence.NewMemorySequencer(), 1024, 5, false)
	topo.UpAdjustMaxVolumeId(needle.VolumeId(17))
	state, err := massdblicense.EmptyState().Bootstrap(false, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := topo.MassDBLicense.Restore(state); err != nil {
		t.Fatal(err)
	}
	machine := StateMachine{topo: topo}
	bytes, err := machine.Save()
	if err != nil {
		t.Fatal(err)
	}

	restoredTopo := topology.NewTopology("restored", sequence.NewMemorySequencer(), 1024, 5, false)
	restored := StateMachine{topo: restoredTopo}
	if err := restored.Recovery(bytes); err != nil {
		t.Fatal(err)
	}
	if restoredTopo.GetMaxVolumeId() != needle.VolumeId(17) {
		t.Fatalf("max volume id=%d", restoredTopo.GetMaxVolumeId())
	}
	if restoredTopo.MassDBLicense.Snapshot().LicenseControlDeploymentUUID !=
		state.LicenseControlDeploymentUUID {
		t.Fatal("MassDB License state was not restored")
	}
}

func TestLegacySnapshotRemainsReadable(t *testing.T) {
	topo := topology.NewTopology("legacy", sequence.NewMemorySequencer(), 1024, 5, false)
	machine := StateMachine{topo: topo}
	if err := machine.Recovery([]byte(`{"maxVolumeId":23}`)); err != nil {
		t.Fatal(err)
	}
	if topo.GetMaxVolumeId() != needle.VolumeId(23) ||
		topo.MassDBLicense.Snapshot().EnforcementMode != massdblicense.ModeUninitialized {
		t.Fatal("legacy snapshot compatibility failed")
	}
}

func TestHashicorpEnvelopeRejectsUnknownCommand(t *testing.T) {
	topo := topology.NewTopology("hashicorp", sequence.NewMemorySequencer(), 1024, 5, false)
	machine := StateMachine{topo: topo}
	bytes, _ := json.Marshal(map[string]any{"formatVersion": 1, "commandType": "UNKNOWN"})
	result := machine.Apply(&hashicorpRaft.Log{Data: bytes})
	if result == nil {
		t.Fatal("unknown command must fail closed")
	}
}
