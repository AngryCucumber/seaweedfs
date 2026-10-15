package mount

import (
	"errors"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestFileHandleReadFailure(t *testing.T) {
	fh := &FileHandle{}
	if fh.readError() != nil {
		t.Fatal("new handle fails")
	}

	// A manifest that failed to resolve waits the retry interval.
	resolveErr := errors.New("license: NO_LICENSE")
	fh.setEntryReadFailure(&filer_pb.Entry{}, resolveErr)
	if f := fh.readFailure.Load(); f == nil || f.err != resolveErr || time.Since(f.at) > time.Second {
		t.Fatalf("manifest failure %+v", f)
	}
	fh.maybeRetryRead() // within the interval: no attempt, no wfs needed
	if fh.readError() != resolveErr {
		t.Fatal("retried within the interval")
	}

	// Hidden content may come from a stale meta cache: retry at once.
	entry := &filer_pb.Entry{ContentHidden: true}
	fh.setEntryReadFailure(entry, nil)
	if f := fh.readFailure.Load(); f == nil || f.err != filer_pb.ErrContentHidden || !f.at.IsZero() {
		t.Fatalf("hidden failure %+v", f)
	}

	// A write drops inline content, so nothing is hidden any more.
	fh.clearHiddenContent(entry)
	if entry.ContentHidden || fh.readError() != nil {
		t.Fatalf("after write: hidden %v, err %v", entry.ContentHidden, fh.readError())
	}
	fh.setEntryReadFailure(&filer_pb.Entry{}, resolveErr)
	fh.clearHiddenContent(&filer_pb.Entry{ContentHidden: true})
	if fh.readError() != resolveErr {
		t.Fatal("a write cleared a manifest failure")
	}

	fh.setEntryReadFailure(&filer_pb.Entry{}, nil)
	if fh.readError() != nil {
		t.Fatal("resolved entry still fails")
	}
}

func TestTakeRefetchedEntry(t *testing.T) {
	hidden := func() *filer_pb.Entry {
		return &filer_pb.Entry{
			Name:          "a",
			ContentHidden: true,
			Attributes:    &filer_pb.FuseAttributes{FileMode: 0644, Uid: 1000, Gid: 1000, FileSize: 5},
		}
	}
	fresh := func() *filer_pb.Entry {
		return &filer_pb.Entry{
			Name:       "a",
			Content:    []byte("hello"),
			Attributes: &filer_pb.FuseAttributes{FileMode: 0644, Uid: 1000, Gid: 1000, FileSize: 5},
		}
	}

	// chmod, chown and setfattr on the handle while the content was hidden,
	// not flushed yet: they survive, only the content comes from the filer.
	local := hidden()
	local.Attributes.FileMode, local.Attributes.Uid = 0600, 2000
	local.Extended = map[string][]byte{"user.k": []byte("v")}
	fh := &FileHandle{entry: &LockedEntry{Entry: local}, dirtyMetadata: true}
	if fh.takeRefetchedEntry(fresh(), nil) {
		t.Fatal("the filer entry replaced unflushed local changes")
	}
	e := fh.entry.GetEntry()
	if e.Attributes.FileMode != 0600 || e.Attributes.Uid != 2000 || string(e.Extended["user.k"]) != "v" {
		t.Fatalf("local changes lost: %+v", e)
	}
	if string(e.Content) != "hello" || e.ContentHidden {
		t.Fatalf("content not taken: %+v", e)
	}

	// A truncation from 5 to 3 bytes not flushed yet keeps its size.
	local = hidden()
	local.Attributes.FileSize = 3
	fh = &FileHandle{entry: &LockedEntry{Entry: local}, dirtyMetadata: true}
	fh.takeRefetchedEntry(fresh(), nil)
	if e := fh.entry.GetEntry(); string(e.Content) != "hel" || e.Attributes.FileSize != 3 {
		t.Fatalf("truncated entry: size %d, content %q", e.Attributes.FileSize, e.Content)
	}

	// Without local changes the filer entry is taken as a whole.
	fh = &FileHandle{entry: &LockedEntry{Entry: hidden()}}
	if !fh.takeRefetchedEntry(fresh(), nil) {
		t.Fatal("clean handle kept the hidden entry")
	}
	if e := fh.entry.GetEntry(); string(e.Content) != "hello" || e.ContentHidden {
		t.Fatalf("entry %+v", e)
	}
}

func TestLocalEntryFromFiler(t *testing.T) {
	mapper, err := meta_cache.NewUidGidMapper("2000:1000", "2000:1000")
	if err != nil {
		t.Fatal(err)
	}
	wfs := &WFS{option: &Option{UidGidMapper: mapper}}
	fresh := &filer_pb.Entry{Name: "a", Content: []byte("hello"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, Uid: 1000, Gid: 1000}}
	cached := wfs.localEntryFromFiler("/d", fresh)
	if fresh.Attributes.Uid != 2000 || fresh.Attributes.Gid != 2000 {
		t.Fatalf("handle entry keeps filer ids %d:%d", fresh.Attributes.Uid, fresh.Attributes.Gid)
	}
	if cached.Attr.Uid != 1000 || cached.Attr.Gid != 1000 || cached.FullPath != "/d/a" {
		t.Fatalf("meta cache entry %s %d:%d, want filer ids", cached.FullPath, cached.Attr.Uid, cached.Attr.Gid)
	}
}

// testWFS is a mount with handles kept in memory; nothing reaches a filer.
func testWFS(t *testing.T) *WFS {
	return &WFS{
		option:      &Option{ChunkSizeLimit: 1 << 20, ConcurrentWriters: 4, uniqueCacheDirForWrite: t.TempDir()},
		inodeToPath: NewInodeToPath("/", 0),
		fhMap:       NewFileHandleToInode(),
		fhLockTable: util.NewLockTable[FileHandleId](),
	}
}

func (wfs *WFS) testOpen(path string, entry *filer_pb.Entry) *FileHandle {
	inode := wfs.inodeToPath.Lookup(util.FullPath(path), time.Now().Unix(), false, false, 0, true)
	return wfs.fhMap.AcquireFileHandle(wfs, inode, entry)
}

func hiddenEntry(name string, size uint64) *filer_pb.Entry {
	return &filer_pb.Entry{Name: name, ContentHidden: true, Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: size}}
}

func readHandle(t *testing.T, fh *FileHandle, size int) string {
	t.Helper()
	buf := make([]byte, size)
	n, err := readDataByFileHandle(buf, fh, 0)
	if err != nil {
		t.Fatalf("read %s: %v", fh.FullPath(), err)
	}
	return string(buf[:n])
}

func TestCopyFileRangeClearsHiddenContent(t *testing.T) {
	wfs := testWFS(t)
	src := wfs.testOpen("/d/src", &filer_pb.Entry{Name: "src", Content: []byte("world"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}})
	dst := wfs.testOpen("/d/dst", hiddenEntry("dst", 5))

	written, code := wfs.CopyFileRange(nil, &fuse.CopyFileRangeIn{FhIn: uint64(src.fh), FhOut: uint64(dst.fh), Len: 5})
	if code != fuse.OK || written != 5 {
		t.Fatalf("copy: %v %d", code, written)
	}
	// Like a write, the copy replaced what was hidden: the content must not
	// be fetched back over it.
	if dst.entry.GetEntry().ContentHidden || dst.readError() != nil {
		t.Fatalf("hidden %v, read error %v", dst.entry.GetEntry().ContentHidden, dst.readError())
	}
	if got := readHandle(t, dst, 5); got != "world" {
		t.Fatalf("read %q", got)
	}
}

func TestTruncateHiddenContent(t *testing.T) {
	setSize := func(wfs *WFS, fh *FileHandle, size uint64) {
		t.Helper()
		in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: fuse.InHeader{NodeId: fh.inode}, Valid: fuse.FATTR_SIZE | fuse.FATTR_FH, Fh: uint64(fh.fh), Size: size}}
		if code := wfs.SetAttr(nil, in, &fuse.AttrOut{}); code != fuse.OK {
			t.Fatalf("truncate to %d: %v", size, code)
		}
	}
	fresh := func() *filer_pb.Entry {
		return &filer_pb.Entry{Name: "a", Content: []byte("hello"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}
	}

	// Cut to 3 bytes, grown back to 5: the filer still has 5 bytes and
	// would fill them all back in, so the flush is refused; content that
	// comes back has zeros past the cut.
	wfs := testWFS(t)
	fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
	setSize(wfs, fh, 3)
	setSize(wfs, fh, 4)
	setSize(wfs, fh, 5)
	if err := fh.hiddenFlushError(); !errors.Is(err, filer_pb.ErrContentHidden) {
		t.Fatalf("flush error %v", err)
	}
	if code := wfs.doFlush(fh, 0, 0); code != fuse.EIO {
		t.Fatalf("flush: %v", code)
	}
	fh.takeRefetchedEntry(fresh(), nil)
	fh.setReadFailure(nil) // as maybeRetryRead does after the refetch
	if e := fh.entry.GetEntry(); string(e.Content) != "hel\x00\x00" || e.Attributes.FileSize != 5 || e.ContentHidden {
		t.Fatalf("content %q, size %d, hidden %v", e.Content, e.Attributes.FileSize, e.ContentHidden)
	}
	if got := readHandle(t, fh, 8); got != "hel\x00\x00" {
		t.Fatalf("read %q", got)
	}
	if err := fh.hiddenFlushError(); err != nil {
		t.Fatalf("flush error after the content came back: %v", err)
	}

	// Grown first and cut back to the stored size: nothing was dropped.
	wfs = testWFS(t)
	fh = wfs.testOpen("/d/a", hiddenEntry("a", 5))
	setSize(wfs, fh, 7)
	setSize(wfs, fh, 5)
	if err := fh.hiddenFlushError(); err != nil {
		t.Fatalf("flush error %v", err)
	}
	fh.takeRefetchedEntry(fresh(), nil)
	if e := fh.entry.GetEntry(); string(e.Content) != "hello" {
		t.Fatalf("content %q", e.Content)
	}

	// Cut to 0 drops all hidden content, so growing again is plain zeros.
	wfs = testWFS(t)
	fh = wfs.testOpen("/d/a", hiddenEntry("a", 5))
	setSize(wfs, fh, 2)
	setSize(wfs, fh, 0)
	setSize(wfs, fh, 5)
	if e := fh.entry.GetEntry(); e.ContentHidden || fh.hiddenCut.Load() != 0 || fh.hiddenFlushError() != nil {
		t.Fatalf("hidden %v, cut %d", e.ContentHidden, fh.hiddenCut.Load())
	}
}
