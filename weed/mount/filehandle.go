package mount

import (
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type FileHandleId uint64

var IsDebugFileReadWrite = false

type FileHandle struct {
	fh              FileHandleId
	counter         int64
	entry           *LockedEntry
	entryLock       sync.RWMutex
	entryChunkGroup *filer.ChunkGroup
	inode           uint64
	wfs             *WFS

	// cache file has been written to
	dirtyMetadata bool
	dirtyPages    *PageWriter
	reader        *filer.ChunkReadAt
	contentType   string

	isDeleted bool

	// readFailure is set while reads fail with EIO; readRetryLock lets one
	// reader at a time try to clear it (filehandle_license.go).
	readFailure   atomic.Pointer[readFailure]
	readRetryLock sync.Mutex
	// hiddenCut is the smallest size a truncation on this handle left a
	// file with hidden inline content at, 0 if there was none; hiddenSize
	// is the size the entry had when it was set with hidden content.
	hiddenCut  atomic.Uint64
	hiddenSize atomic.Uint64
	// cachedRemote holds the chunks and remote object of this handle's
	// last download of a remote object until the filer's event for it
	// arrives; remoteDownloadLock lets one download run at a time
	// (filehandle_read.go).
	cachedRemote       atomic.Pointer[filer_pb.Entry]
	remoteDownloadLock sync.Mutex
	// lastWriteTsNs is the timestamp of the last write on this handle
	// (writeTsNs); used under the handle's exclusive lock. chunkTsNs is at
	// least the newest timestamp of the chunks the entry has had, raised
	// whenever chunks come in (noteChunks); it only grows, so a write
	// needs no look at the chunks, which uploads append concurrently.
	lastWriteTsNs int64
	chunkTsNs     atomic.Int64

	// for debugging
	mirrorFile *os.File
}

func newFileHandle(wfs *WFS, handleId FileHandleId, inode uint64, entry *filer_pb.Entry) *FileHandle {
	fh := &FileHandle{
		fh:      handleId,
		counter: 1,
		inode:   inode,
		wfs:     wfs,
	}
	// dirtyPages: newContinuousDirtyPages(file, writeOnly),
	fh.dirtyPages = newPageWriter(fh, wfs.option.ChunkSizeLimit)
	fh.entry = &LockedEntry{
		Entry: entry,
	}
	if entry != nil {
		fh.SetEntry(entry)
	}

	if IsDebugFileReadWrite {
		var err error
		fh.mirrorFile, err = os.OpenFile("/tmp/sw/"+entry.Name, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			println("failed to create mirror:", err.Error())
		}
	}

	return fh
}

func (fh *FileHandle) FullPath() util.FullPath {
	fp, _ := fh.wfs.inodeToPath.GetPath(fh.inode)
	return fp
}

func (fh *FileHandle) GetEntry() *LockedEntry {
	return fh.entry
}

func (fh *FileHandle) SetEntry(entry *filer_pb.Entry) {
	if entry != nil {
		fileSize := filer.FileSize(entry)
		entry.Attributes.FileSize = fileSize
		var resolveManifestErr error
		fh.entryChunkGroup, resolveManifestErr = filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, entry.Chunks)
		if resolveManifestErr != nil {
			glog.Warningf("failed to resolve manifest chunks in %s: %v", entry.Name, resolveManifestErr)
		}
		fh.noteChunks(entry.Chunks, fh.entryChunkGroup)
		// Reads fail instead of returning zeros until the chunks resolve
		// or the content is no longer hidden (plan 1.6).
		fh.setEntryReadFailure(entry, resolveManifestErr)
		fh.hiddenCut.Store(0)
		fh.cachedRemote.Store(nil)
		if entry.ContentHidden {
			fh.hiddenSize.Store(fileSize)
		}
	} else {
		glog.Fatalf("setting file handle entry to nil")
	}
	fh.entry.SetEntry(entry)
}

func (fh *FileHandle) UpdateEntry(fn func(entry *filer_pb.Entry)) *filer_pb.Entry {
	return fh.entry.UpdateEntry(fn)
}

// writeTsNs returns the timestamp of a write on this handle. Overlapping
// chunks are resolved by timestamp, so a write must be newer than every
// chunk of the entry, whose timestamps may come from the filer's clock (a
// remote object it cached, an entry it returned), and than the previous
// write on the handle, which may already have been moved past such a
// chunk. Called with the handle's exclusive lock, before the write.
func (fh *FileHandle) writeTsNs() int64 {
	tsNs := time.Now().UnixNano()
	if fh.lastWriteTsNs >= tsNs {
		tsNs = fh.lastWriteTsNs + 1
	}
	if chunkTsNs := fh.chunkTsNs.Load(); chunkTsNs >= tsNs {
		tsNs = chunkTsNs + 1
	}
	fh.lastWriteTsNs = tsNs
	return tsNs
}

// noteChunks raises chunkTsNs to the newest timestamp of chunks and of
// group, the chunk group built from them with manifests resolved. Called
// wherever chunks come into the entry.
func (fh *FileHandle) noteChunks(chunks []*filer_pb.FileChunk, group *filer.ChunkGroup) {
	var tsNs int64
	for _, chunk := range chunks {
		tsNs = max(tsNs, chunk.ModifiedTsNs)
	}
	if group != nil {
		tsNs = max(tsNs, group.LatestModifiedTsNs())
	}
	for {
		current := fh.chunkTsNs.Load()
		if tsNs <= current || fh.chunkTsNs.CompareAndSwap(current, tsNs) {
			return
		}
	}
}

func (fh *FileHandle) AddChunks(chunks []*filer_pb.FileChunk) {
	fh.noteChunks(chunks, nil)
	fh.entry.AppendChunks(chunks)
}

func (fh *FileHandle) ReleaseHandle() {

	fhActiveLock := fh.wfs.fhLockTable.AcquireLock("ReleaseHandle", fh.fh, util.ExclusiveLock)
	defer fh.wfs.fhLockTable.ReleaseLock(fh.fh, fhActiveLock)

	fh.dirtyPages.Destroy()
	if IsDebugFileReadWrite {
		fh.mirrorFile.Close()
	}
}

func lessThan(a, b *filer_pb.FileChunk) bool {
	if a.ModifiedTsNs == b.ModifiedTsNs {
		return a.Fid.FileKey < b.Fid.FileKey
	}
	return a.ModifiedTsNs < b.ModifiedTsNs
}
