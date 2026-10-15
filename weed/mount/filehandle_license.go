package mount

import (
	"context"
	"fmt"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// readRetryInterval is the least time between two attempts to leave the
// read failure state of a file handle (plan 1.6).
const readRetryInterval = 5 * time.Second

// readFailure makes reads of a file handle fail with EIO instead of
// returning zeros: its manifest chunks could not be resolved, typically
// because the volume servers deny reads without a valid license, or the
// filer hid its inline content (filer_pb.ErrContentHidden).
type readFailure struct {
	err error
	at  time.Time
}

// setEntryReadFailure records the read failure of a newly set entry. An
// entry with hidden content may come from the meta cache, which keeps it
// after the license is renewed, so the first read asks the filer again
// right away; a manifest that just failed to resolve waits the interval.
func (fh *FileHandle) setEntryReadFailure(entry *filer_pb.Entry, resolveManifestErr error) {
	if entry.ContentHidden {
		fh.readFailure.Store(&readFailure{err: filer_pb.ErrContentHidden})
		return
	}
	fh.setReadFailure(resolveManifestErr)
}

func (fh *FileHandle) setReadFailure(err error) {
	if err == nil {
		fh.readFailure.Store(nil)
		return
	}
	fh.readFailure.Store(&readFailure{err: err, at: time.Now()})
}

// readError returns the error reads of this handle fail with, or nil.
// Normal reads pay one atomic load.
func (fh *FileHandle) readError() error {
	if f := fh.readFailure.Load(); f != nil {
		return f.err
	}
	return nil
}

// clearHiddenContent is called when a write drops the inline content, as
// mount writes always do: nothing is hidden from this handle any more.
func (fh *FileHandle) clearHiddenContent(entry *filer_pb.Entry) {
	if !entry.ContentHidden {
		return
	}
	entry.ContentHidden = false
	if f := fh.readFailure.Load(); f != nil && f.err == filer_pb.ErrContentHidden {
		fh.readFailure.CompareAndSwap(f, nil)
	}
}

// maybeRetryRead tries to leave the read failure state at most once per
// readRetryInterval: it resolves the manifest chunks again, or fetches an
// entry whose content was hidden from the filer again. Only one attempt
// runs at a time; readers that find one running keep failing.
func (fh *FileHandle) maybeRetryRead() {
	f := fh.readFailure.Load()
	if f == nil || time.Since(f.at) < readRetryInterval {
		return
	}
	if !fh.readRetryLock.TryLock() {
		return
	}
	defer fh.readRetryLock.Unlock()
	if f = fh.readFailure.Load(); f == nil || time.Since(f.at) < readRetryInterval {
		return
	}

	var err error
	if f.err == filer_pb.ErrContentHidden {
		err = fh.refetchHiddenEntry()
	} else {
		err = fh.resolveChunksAgain()
	}
	if err != nil {
		glog.V(1).Infof("retry reading %s: %v", fh.FullPath(), err)
	}
	fh.setReadFailure(err)
}

// resolveChunksAgain rebuilds the chunk group from the current entry.
// Chunks appended by writes while it ran are added to the new group, so
// none is lost.
func (fh *FileHandle) resolveChunksAgain() error {
	chunks := fh.entry.GetChunks()
	group, err := filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, chunks)
	if err != nil {
		return err
	}
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	current := fh.entry.GetChunks()
	if len(current) < len(chunks) {
		return fmt.Errorf("chunks changed while resolving")
	}
	for _, chunk := range current[len(chunks):] {
		group.AddChunk(chunk)
	}
	fh.entryChunkGroup = group
	return nil
}

// refetchHiddenEntry looks the entry up again. Once the filer no longer
// hides its content, the handle takes it (takeRefetchedEntry), and the
// meta cache takes the new entry unless the handle keeps local changes.
func (fh *FileHandle) refetchHiddenEntry() error {
	fullPath := fh.FullPath()
	fresh, err := filer_pb.GetEntry(fh.wfs, fullPath)
	if err != nil {
		return err
	}
	if fresh == nil {
		return filer_pb.ErrNotFound
	}
	if fresh.ContentHidden {
		return filer_pb.ErrContentHidden
	}
	chunks := fresh.Chunks
	if fh.dirtyMetadata {
		chunks = fh.entry.GetChunks()
	}
	group, err := filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, chunks)
	if err != nil {
		return err
	}
	dir, _ := fullPath.DirAndName()
	cached := fh.wfs.localEntryFromFiler(dir, fresh)
	if !fh.takeRefetchedEntry(fresh, group) {
		return nil
	}
	if err := fh.wfs.metaCache.UpdateEntry(context.Background(), cached); err != nil {
		glog.V(1).Infof("update meta cache %s: %v", fullPath, err)
	}
	return nil
}

// localEntryFromFiler returns the meta cache entry for an entry fetched
// from the filer, which keeps the filer's uid and gid, and maps the entry
// itself to the local ids that file handles hold (the meta cache maps them
// on reads the same way).
func (wfs *WFS) localEntryFromFiler(dir string, entry *filer_pb.Entry) *filer.Entry {
	cached := filer.FromPbEntry(dir, entry)
	wfs.mapPbIdFromFilerToLocal(entry)
	return cached
}

// takeRefetchedEntry installs an entry whose content the filer no longer
// hides, already mapped to local ids. chmod, chown, utimes, truncate or
// xattr changes made on this handle while the content was hidden are not
// flushed yet; they stay, and only the content is taken from the filer, cut
// to the local size. It reports whether the filer entry replaced the local
// one.
func (fh *FileHandle) takeRefetchedEntry(fresh *filer_pb.Entry, group *filer.ChunkGroup) bool {
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	fh.entryChunkGroup = group
	if fh.dirtyMetadata {
		fh.entry.UpdateEntry(func(e *filer_pb.Entry) {
			content := fresh.Content
			// A truncation not flushed yet keeps its size.
			if e.Attributes != nil && e.Attributes.FileSize < uint64(len(content)) {
				content = content[:e.Attributes.FileSize]
			}
			e.Content = content
			e.ContentHidden = false
		})
		return false
	}
	fresh.Attributes.FileSize = filer.FileSize(fresh)
	fh.entry.SetEntry(fresh)
	return true
}
