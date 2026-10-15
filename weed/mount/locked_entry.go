package mount

import (
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"sync"
)

type LockedEntry struct {
	*filer_pb.Entry
	sync.RWMutex
}

func (le *LockedEntry) GetEntry() *filer_pb.Entry {
	le.RLock()
	defer le.RUnlock()
	return le.Entry
}

// SetEntry sets the entry of the LockedEntry
// entry should never be nil
func (le *LockedEntry) SetEntry(entry *filer_pb.Entry) {
	le.Lock()
	defer le.Unlock()
	le.Entry = entry
}

func (le *LockedEntry) UpdateEntry(fn func(entry *filer_pb.Entry)) *filer_pb.Entry {
	le.Lock()
	defer le.Unlock()
	fn(le.Entry)
	return le.Entry
}

// IsInRemoteOnly reports whether the content is only in remote storage,
// read under the lock uploads append chunks with.
func (le *LockedEntry) IsInRemoteOnly() bool {
	le.RLock()
	defer le.RUnlock()
	return le.Entry.IsInRemoteOnly()
}

func (le *LockedEntry) GetChunks() []*filer_pb.FileChunk {
	le.RLock()
	defer le.RUnlock()
	return le.Entry.Chunks
}

func (le *LockedEntry) AppendChunks(newChunks []*filer_pb.FileChunk) {
	le.Lock()
	defer le.Unlock()
	le.Entry.Chunks = append(le.Entry.Chunks, newChunks...)
}
