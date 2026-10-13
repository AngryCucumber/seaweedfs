package weed_server

import (
	"context"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
)

func TestIsCopyFileExtAllowed(t *testing.T) {
	for _, ext := range []string{".dat", ".idx", ".vif"} {
		if !isCopyFileExtAllowed(ext, false) {
			t.Errorf("volume ext %q should be allowed", ext)
		}
	}
	for _, ext := range []string{".ec00", ".ec13", ".ecx", ".ecj", ".vif"} {
		if !isCopyFileExtAllowed(ext, true) {
			t.Errorf("ec ext %q should be allowed", ext)
		}
	}
	for _, ext := range []string{"", ".ecx", ".note", ".cpd", ".idx/../../etc/passwd", "/../../etc/passwd"} {
		if isCopyFileExtAllowed(ext, false) {
			t.Errorf("volume ext %q should be rejected", ext)
		}
	}
	for _, ext := range []string{"", ".dat", ".idx", ".ec14", ".ec1", ".ecx/../../etc/passwd"} {
		if isCopyFileExtAllowed(ext, true) {
			t.Errorf("ec ext %q should be rejected", ext)
		}
	}
}

// The handlers must reject unsafe names before touching the store, so a bare
// VolumeServer is enough here.
func TestVolumeGrpcRejectsPathTraversal(t *testing.T) {
	vs := &VolumeServer{}
	ctx := context.Background()

	copyRequests := []*volume_server_pb.CopyFileRequest{
		{VolumeId: 1, Ext: "/../../../etc/passwd"},
		{VolumeId: 1, Ext: ".idx/../../etc/passwd"},
		{VolumeId: 1, Ext: ".ecx", Collection: "../../../etc", IsEcVolume: true},
		{VolumeId: 1, Ext: "/../../etc/passwd", IsEcVolume: true},
	}
	for _, req := range copyRequests {
		if err := vs.CopyFile(req, nil); err == nil {
			t.Errorf("CopyFile accepted %+v", req)
		}
	}

	const badCollection = "../../tmp/x"
	if _, err := vs.VolumeEcShardsRebuild(ctx, &volume_server_pb.VolumeEcShardsRebuildRequest{VolumeId: 1, Collection: badCollection}); err == nil {
		t.Error("VolumeEcShardsRebuild accepted a traversal collection")
	}
	if _, err := vs.VolumeEcShardsCopy(ctx, &volume_server_pb.VolumeEcShardsCopyRequest{VolumeId: 1, Collection: badCollection}); err == nil {
		t.Error("VolumeEcShardsCopy accepted a traversal collection")
	}
	if _, err := vs.VolumeEcShardsDelete(ctx, &volume_server_pb.VolumeEcShardsDeleteRequest{VolumeId: 1, Collection: badCollection}); err == nil {
		t.Error("VolumeEcShardsDelete accepted a traversal collection")
	}
}
