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

// lookupEntry fetches an entry from the filer; tests replace it.
var lookupEntry = filer_pb.GetEntry

// readFailure makes reads of a file handle fail with EIO instead of
// returning zeros: its manifest chunks could not be resolved, typically
// because the volume servers deny reads without a valid license, or the
// filer hid its inline content (filer_pb.ErrContentHidden).
type readFailure struct {
	err error
	at  time.Time
	// hidden is set while the inline content is hidden. It decides the
	// retry, not err: a failed lookup of the entry replaces err but the
	// content stays hidden until the lookup succeeds or a write drops it.
	hidden bool
}

// setEntryReadFailure records the read failure of a newly set entry. An
// entry with hidden content may come from the meta cache, which keeps it
// after the license is renewed, so the first read asks the filer again
// right away; a manifest that just failed to resolve waits the interval.
func (fh *FileHandle) setEntryReadFailure(entry *filer_pb.Entry, resolveManifestErr error) {
	if entry.ContentHidden {
		fh.readFailure.Store(&readFailure{err: filer_pb.ErrContentHidden, hidden: true})
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
	fh.hiddenCut.Store(0)
	if f := fh.readFailure.Load(); f != nil && f.hidden {
		fh.readFailure.CompareAndSwap(f, nil)
	}
}

// truncateHidden records a truncation to a size above 0 of a file whose
// inline content is hidden. The bytes past the smallest such size are gone
// even if the file grows again before the content comes back, and only the
// handle knows it: the filer fills a hidden entry of unchanged size back in
// with all of its stored bytes.
func (fh *FileHandle) truncateHidden(size uint64) {
	if cut := fh.hiddenCut.Load(); cut == 0 || size < cut {
		fh.hiddenCut.Store(size)
	}
}

// hiddenFlushError refuses to flush an entry whose hidden content was
// truncated on this handle and grew again, as the filer would bring the
// dropped bytes back. Other size changes of hidden content the filer
// refuses itself (CONTENT_HIDDEN).
func (fh *FileHandle) hiddenFlushError() error {
	entry := fh.GetEntry().GetEntry()
	if entry == nil || !entry.ContentHidden {
		return nil
	}
	if cut, size := fh.hiddenCut.Load(), entry.Attributes.GetFileSize(); cut != 0 && cut < size {
		return fmt.Errorf("truncated to %d bytes, now %d: %w", cut, size, filer_pb.ErrContentHidden)
	}
	return nil
}

// maybeRetryRead tries to leave the read failure state at most once per
// readRetryInterval: it resolves the manifest chunks again, or fetches an
// entry whose content was hidden from the filer again. Only one attempt
// runs at a time; readers that find one running keep failing.
//
// The failure f the attempt started from also marks the handle state it
// belongs to: a truncation to 0 (clearHiddenContent) or a new entry
// (SetEntry) replaces it while the attempt runs, and the attempt's result
// is then dropped instead of bringing back old content or a cleared error.
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
	if f.hidden {
		err = fh.refetchHiddenEntry(f)
	} else {
		err = fh.resolveChunksAgain(f)
	}
	if err != nil {
		glog.V(1).Infof("retry reading %s: %v", fh.FullPath(), err)
	}
	var next *readFailure
	if err != nil {
		next = &readFailure{err: err, at: time.Now(), hidden: f.hidden}
	}
	fh.readFailure.CompareAndSwap(f, next)
}

// resolveChunksAgain rebuilds the chunk group from the current entry.
// Chunks appended by writes while it ran are added to the new group, so
// none is lost.
func (fh *FileHandle) resolveChunksAgain(f *readFailure) error {
	chunks := fh.entry.GetChunks()
	group, err := filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, chunks)
	if err != nil {
		return err
	}
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	if fh.readFailure.Load() != f {
		return nil // a new entry came with its own chunk group
	}
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
func (fh *FileHandle) refetchHiddenEntry(f *readFailure) error {
	fullPath := fh.FullPath()
	fresh, err := lookupEntry(fh.wfs, fullPath)
	if err != nil {
		return err
	}
	if fresh == nil {
		return filer_pb.ErrNotFound
	}
	if fresh.ContentHidden {
		return filer_pb.ErrContentHidden
	}
	group, err := filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, fresh.Chunks)
	if err != nil {
		return err
	}
	dir, _ := fullPath.DirAndName()
	cached := fh.wfs.localEntryFromFiler(dir, fresh)
	if replaced, err := fh.takeRefetchedEntry(f, fresh, group); err != nil || !replaced {
		return err
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
// hides, already mapped to local ids, and the chunk group of its chunks.
// chmod, chown, utimes, truncate or xattr changes made on this handle while
// the content was hidden are not flushed yet; they stay, and only the
// content is taken from the filer (takeContent). It reports whether the
// filer entry replaced the local one. Nothing is taken once the failure f
// the lookup started from was replaced (maybeRetryRead).
func (fh *FileHandle) takeRefetchedEntry(f *readFailure, fresh *filer_pb.Entry, group *filer.ChunkGroup) (bool, error) {
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	if fh.readFailure.Load() != f || !fh.entry.GetEntry().GetContentHidden() {
		return false, nil
	}
	replaced := !fh.dirtyMetadata
	if replaced {
		fresh.Attributes.FileSize = filer.FileSize(fresh)
		fh.entry.SetEntry(fresh)
	} else {
		var err error
		fh.entry.UpdateEntry(func(e *filer_pb.Entry) {
			err = fh.takeContent(e, fresh)
		})
		if err != nil {
			return false, err
		}
	}
	fh.entryChunkGroup = group
	fh.hiddenCut.Store(0)
	return replaced, nil
}

// takeContent puts the content of fresh into the local entry e and keeps
// the rest of e. Without a size change on this handle the filer's inline
// bytes, chunks, remote object, size and md5 are taken together, whatever
// another client wrote meanwhile. After a truncation or extension only
// inline bytes can be cut to the local size (keptContent); chunks or a
// remote object that the filer holds now cannot be, so the content stays
// hidden and a flush is refused by the filer (CONTENT_HIDDEN) instead of
// dropping them.
func (fh *FileHandle) takeContent(e, fresh *filer_pb.Entry) error {
	cut := fh.hiddenCut.Load()
	if cut == 0 && e.Attributes.FileSize == fh.hiddenSize.Load() {
		e.Content, e.Chunks, e.RemoteEntry = fresh.Content, fresh.Chunks, fresh.RemoteEntry
		e.Attributes.FileSize = filer.FileSize(fresh)
		e.Attributes.Md5 = fresh.GetAttributes().GetMd5()
	} else if len(fresh.Chunks) > 0 || fresh.RemoteEntry != nil {
		return fmt.Errorf("size changed on this handle while the content, now in chunks or a remote object, was hidden: %w", filer_pb.ErrContentHidden)
	} else {
		e.Content, e.Chunks, e.RemoteEntry = keptContent(fresh.Content, e.Attributes.FileSize, cut), nil, nil
	}
	e.ContentHidden = false
	return nil
}

// keptContent returns the content taken back from the filer as truncations
// not flushed yet left it: cut to the local size, and zero from the
// smallest size the handle truncated it to (truncateHidden).
func keptContent(content []byte, size, cut uint64) []byte {
	if size < uint64(len(content)) {
		content = content[:size]
	}
	if cut == 0 || cut >= uint64(len(content)) {
		return content
	}
	kept := make([]byte, len(content))
	copy(kept, content[:cut])
	return kept
}
