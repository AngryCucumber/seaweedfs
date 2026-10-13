package storage

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/erasure_coding"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestValidateCollectionName(t *testing.T) {
	for _, name := range []string{"", "bucket", "my-bucket.v2", "a_b", ".", ".."} {
		if err := ValidateCollectionName(name); err != nil {
			t.Errorf("collection %q should be valid: %v", name, err)
		}
	}
	for _, name := range []string{"../etc", "a/b", "/abs", `a\b`, "a\x00b", "../../tmp/x"} {
		if err := ValidateCollectionName(name); err == nil {
			t.Errorf("collection %q should be rejected", name)
		}
	}
}

func TestStoreRejectsTraversalCollection(t *testing.T) {
	s := &Store{}
	if err := s.AddVolume(needle.VolumeId(1), "../../tmp/x", NeedleMapInMemory, "000", "", 0, 0, types.HardDriveType, 0); err == nil {
		t.Error("AddVolume accepted a collection with path separators")
	}
	if err := s.MountEcShards("../../tmp/x", needle.VolumeId(1), erasure_coding.ShardId(0)); err == nil {
		t.Error("MountEcShards accepted a collection with path separators")
	}
}
