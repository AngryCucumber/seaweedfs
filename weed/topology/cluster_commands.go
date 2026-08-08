package topology

import (
	"encoding/json"
	"fmt"
	hashicorpRaft "github.com/hashicorp/raft"
	"github.com/seaweedfs/raft"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/massdblicense"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

const MassDBLicenseCommandType = "MASSDB_LICENSE_STATE"

// MasterSnapshot is versioned while remaining backward compatible with the legacy
// {"maxVolumeId":N} snapshot.
type MasterSnapshot struct {
	FormatVersion int                  `json:"formatVersion,omitempty"`
	MaxVolumeId   needle.VolumeId      `json:"maxVolumeId"`
	MassDBLicense *massdblicense.State `json:"massDbLicense,omitempty"`
}

func (s *MasterSnapshot) Persist(sink hashicorpRaft.SnapshotSink) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal: %v", err)
	}
	if _, err = sink.Write(b); err != nil {
		sink.Cancel()
		return fmt.Errorf("sink.Write(): %v", err)
	}
	return sink.Close()
}

func (s *MasterSnapshot) Release() {}

type MassDBLicenseEnvelope struct {
	FormatVersion int                 `json:"formatVersion"`
	CommandType   string              `json:"commandType"`
	Payload       massdblicense.State `json:"payload"`
}

func NewMassDBLicenseEnvelope(state massdblicense.State) *MassDBLicenseEnvelope {
	return &MassDBLicenseEnvelope{FormatVersion: 1, CommandType: MassDBLicenseCommandType, Payload: state}
}

type MassDBLicenseCommand struct {
	State massdblicense.State `json:"state"`
}

func NewMassDBLicenseCommand(state massdblicense.State) *MassDBLicenseCommand {
	return &MassDBLicenseCommand{State: state}
}

func (c *MassDBLicenseCommand) CommandName() string { return "MassDBLicenseState" }

func (c *MassDBLicenseCommand) Apply(server raft.Server) (interface{}, error) {
	topo := server.Context().(*Topology)
	return nil, topo.MassDBLicense.Restore(c.State)
}

type MaxVolumeIdCommand struct {
	MaxVolumeId needle.VolumeId `json:"maxVolumeId"`
}

func NewMaxVolumeIdCommand(value needle.VolumeId) *MaxVolumeIdCommand {
	return &MaxVolumeIdCommand{
		MaxVolumeId: value,
	}
}

func (c *MaxVolumeIdCommand) CommandName() string {
	return "MaxVolumeId"
}

// deprecatedCommandApply represents the old interface to apply a command to the server.
func (c *MaxVolumeIdCommand) Apply(server raft.Server) (interface{}, error) {
	topo := server.Context().(*Topology)
	before := topo.GetMaxVolumeId()
	topo.UpAdjustMaxVolumeId(c.MaxVolumeId)

	glog.V(1).Infoln("max volume id", before, "==>", topo.GetMaxVolumeId())

	return nil, nil
}

func (s *MaxVolumeIdCommand) Persist(sink hashicorpRaft.SnapshotSink) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal: %v", err)
	}
	_, err = sink.Write(b)
	if err != nil {
		sink.Cancel()
		return fmt.Errorf("sink.Write(): %v", err)
	}
	return sink.Close()
}

func (s *MaxVolumeIdCommand) Release() {
}
