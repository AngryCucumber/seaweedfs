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
	if !fh.takeRefetchedEntry(fresh, group) {
		return nil
	}
	dir, _ := fullPath.DirAndName()
	if err := fh.wfs.metaCache.UpdateEntry(context.Background(), filer.FromPbEntry(dir, fresh)); err != nil {
		glog.V(1).Infof("update meta cache %s: %v", fullPath, err)
	}
	return nil
}

// takeRefetchedEntry installs an entry whose content the filer no longer
// hides. chmod, chown, utimes or xattr changes made on this handle while
// the content was hidden are not flushed yet; they stay, and only the
// content is taken from the filer. It reports whether the filer entry
// replaced the local one.
func (fh *FileHandle) takeRefetchedEntry(fresh *filer_pb.Entry, group *filer.ChunkGroup) bool {
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	fh.entryChunkGroup = group
	if fh.dirtyMetadata {
		fh.entry.UpdateEntry(func(e *filer_pb.Entry) {
			e.Content = fresh.Content
			e.ContentHidden = false
		})
		return false
	}
	fresh.Attributes.FileSize = filer.FileSize(fresh)
	fh.entry.SetEntry(fresh)
	return true
}
