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
	if f := fh.readFailure.Load(); f == nil || f.err != filer_pb.ErrContentHidden || !f.hidden || !f.at.IsZero() {
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
	// handle holds a hidden entry of 5 bytes, as SetEntry leaves it.
	handle := func(local *filer_pb.Entry, dirty bool) *FileHandle {
		fh := &FileHandle{entry: &LockedEntry{Entry: local}, dirtyMetadata: dirty}
		fh.hiddenSize.Store(5)
		return fh
	}
	take := func(fh *FileHandle, fresh *filer_pb.Entry) bool {
		t.Helper()
		replaced, err := fh.takeRefetchedEntry(hide(fh), fresh, nil)
		if err != nil {
			t.Fatal(err)
		}
		return replaced
	}

	// chmod, chown and setfattr on the handle while the content was hidden,
	// not flushed yet: they survive, only the content comes from the filer.
	local := hidden()
	local.Attributes.FileMode, local.Attributes.Uid = 0600, 2000
	local.Extended = map[string][]byte{"user.k": []byte("v")}
	fh := handle(local, true)
	if take(fh, fresh()) {
		t.Fatal("the filer entry replaced unflushed local changes")
	}
	e := fh.entry.GetEntry()
	if e.Attributes.FileMode != 0600 || e.Attributes.Uid != 2000 || string(e.Extended["user.k"]) != "v" {
		t.Fatalf("local changes lost: %+v", e)
	}
	if string(e.Content) != "hello" || e.ContentHidden {
		t.Fatalf("content not taken: %+v", e)
	}

	// Another client rewrote the file meanwhile, in chunks or as longer
	// inline content: the content, chunks, size and md5 are all taken.
	chunked := &filer_pb.Entry{Name: "a", Chunks: []*filer_pb.FileChunk{{FileId: "3,01", Size: 2000}},
		Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 2000, Md5: []byte("md5")}}
	local = hidden()
	local.Attributes.FileMode = 0600
	fh = handle(local, true)
	take(fh, chunked)
	if e := fh.entry.GetEntry(); len(e.Chunks) != 1 || e.Content != nil || e.Attributes.FileSize != 2000 ||
		string(e.Attributes.Md5) != "md5" || e.Attributes.FileMode != 0600 || e.ContentHidden {
		t.Fatalf("chunked rewrite: %+v", e)
	}
	longer := fresh()
	longer.Content, longer.Attributes.FileSize = []byte("world!"), 6
	fh = handle(hidden(), true)
	take(fh, longer)
	if e := fh.entry.GetEntry(); string(e.Content) != "world!" || e.Attributes.FileSize != 6 {
		t.Fatalf("longer inline rewrite: size %d, content %q", e.Attributes.FileSize, e.Content)
	}

	// Or turned it into a remote object: reads must fetch it, not return
	// the empty inline content.
	remoteOnly := &filer_pb.Entry{Name: "a", RemoteEntry: &filer_pb.RemoteEntry{StorageName: "s3", RemoteSize: 5},
		Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}
	local = hidden()
	local.Attributes.FileMode = 0600
	fh = handle(local, true)
	take(fh, remoteOnly)
	if e := fh.entry.GetEntry(); !e.IsInRemoteOnly() || e.Attributes.FileMode != 0600 || e.ContentHidden {
		t.Fatalf("remote rewrite: %+v", e)
	}

	// A truncation from 5 to 3 bytes not flushed yet keeps its size.
	local = hidden()
	local.Attributes.FileSize = 3
	fh = handle(local, true)
	fh.hiddenCut.Store(3)
	take(fh, fresh())
	if e := fh.entry.GetEntry(); string(e.Content) != "hel" || e.Attributes.FileSize != 3 {
		t.Fatalf("truncated entry: size %d, content %q", e.Attributes.FileSize, e.Content)
	}

	// Chunks or a remote object cannot be cut to a local size change: the
	// content stays hidden.
	for _, fresh := range []*filer_pb.Entry{chunked, remoteOnly} {
		local = hidden()
		local.Attributes.FileSize = 3
		fh = handle(local, true)
		fh.hiddenCut.Store(3)
		if _, err := fh.takeRefetchedEntry(hide(fh), fresh, nil); !errors.Is(err, filer_pb.ErrContentHidden) {
			t.Fatalf("merged %+v into a truncated handle: %v", fresh, err)
		}
		if e := fh.entry.GetEntry(); !e.ContentHidden || len(e.Chunks) != 0 || e.RemoteEntry != nil || e.Attributes.FileSize != 3 {
			t.Fatalf("entry changed: %+v", e)
		}
	}

	// Without local changes the filer entry is taken as a whole.
	fh = handle(hidden(), false)
	if !take(fh, fresh()) {
		t.Fatal("clean handle kept the hidden entry")
	}
	if e := fh.entry.GetEntry(); string(e.Content) != "hello" || e.ContentHidden {
		t.Fatalf("entry %+v", e)
	}

	// The failure the lookup started from was replaced meanwhile.
	fh = handle(hidden(), false)
	f := hide(fh)
	hide(fh)
	if replaced, err := fh.takeRefetchedEntry(f, fresh(), nil); replaced || err != nil || !fh.entry.GetEntry().ContentHidden {
		t.Fatal("took an out of date lookup")
	}
}

// hide marks fh as failing reads on hidden content and returns the failure
// a refetch starts from.
func hide(fh *FileHandle) *readFailure {
	f := &readFailure{err: filer_pb.ErrContentHidden, hidden: true}
	fh.readFailure.Store(f)
	return f
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
	mapper, err := meta_cache.NewUidGidMapper("", "")
	if err != nil {
		t.Fatal(err)
	}
	metaCache := meta_cache.NewMetaCache(t.TempDir(), mapper, "/", func(util.FullPath) {}, func(util.FullPath) bool { return true }, func(util.FullPath, *filer_pb.Entry) {})
	t.Cleanup(metaCache.Shutdown)
	return &WFS{
		metaCache:   metaCache,
		option:      &Option{ChunkSizeLimit: 1 << 20, ConcurrentWriters: 4, uniqueCacheDirForWrite: t.TempDir(), UidGidMapper: mapper},
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

func setSize(t *testing.T, wfs *WFS, fh *FileHandle, size uint64) {
	t.Helper()
	in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: fuse.InHeader{NodeId: fh.inode}, Valid: fuse.FATTR_SIZE | fuse.FATTR_FH, Fh: uint64(fh.fh), Size: size}}
	if code := wfs.SetAttr(nil, in, &fuse.AttrOut{}); code != fuse.OK {
		t.Fatalf("truncate to %d: %v", size, code)
	}
}

func TestTruncateHiddenContent(t *testing.T) {
	fresh := func() *filer_pb.Entry {
		return &filer_pb.Entry{Name: "a", Content: []byte("hello"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}
	}

	// Cut to 3 bytes, grown back to 5: the filer still has 5 bytes and
	// would fill them all back in, so the flush is refused; content that
	// comes back has zeros past the cut.
	wfs := testWFS(t)
	fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
	setSize(t, wfs, fh, 3)
	setSize(t, wfs, fh, 4)
	setSize(t, wfs, fh, 5)
	if err := fh.hiddenFlushError(); !errors.Is(err, filer_pb.ErrContentHidden) {
		t.Fatalf("flush error %v", err)
	}
	if code := wfs.doFlush(fh, 0, 0); code != fuse.EIO {
		t.Fatalf("flush: %v", code)
	}
	if _, err := fh.takeRefetchedEntry(fh.readFailure.Load(), fresh(), nil); err != nil {
		t.Fatal(err)
	}
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
	setSize(t, wfs, fh, 7)
	setSize(t, wfs, fh, 5)
	if err := fh.hiddenFlushError(); err != nil {
		t.Fatalf("flush error %v", err)
	}
	if _, err := fh.takeRefetchedEntry(fh.readFailure.Load(), fresh(), nil); err != nil {
		t.Fatal(err)
	}
	if e := fh.entry.GetEntry(); string(e.Content) != "hello" {
		t.Fatalf("content %q", e.Content)
	}

	// Cut to 0 drops all hidden content, so growing again is plain zeros.
	wfs = testWFS(t)
	fh = wfs.testOpen("/d/a", hiddenEntry("a", 5))
	setSize(t, wfs, fh, 2)
	setSize(t, wfs, fh, 0)
	setSize(t, wfs, fh, 5)
	if e := fh.entry.GetEntry(); e.ContentHidden || fh.hiddenCut.Load() != 0 || fh.hiddenFlushError() != nil {
		t.Fatalf("hidden %v, cut %d", e.ContentHidden, fh.hiddenCut.Load())
	}
}

// A lookup still running when the handle is truncated to 0 and grown
// again must not bring back the old content or the cleared read error.
func TestRefetchDroppedAfterTruncate(t *testing.T) {
	defer func() { lookupEntry = filer_pb.GetEntry }()
	for _, filerHides := range []bool{false, true} {
		wfs := testWFS(t)
		fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
		started, release := make(chan struct{}), make(chan struct{})
		lookupEntry = func(filer_pb.FilerClient, util.FullPath) (*filer_pb.Entry, error) {
			close(started)
			<-release
			if filerHides {
				return hiddenEntry("a", 5), nil
			}
			return &filer_pb.Entry{Name: "a", Content: []byte("hello"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}, nil
		}
		done := make(chan struct{})
		go func() {
			fh.maybeRetryRead()
			close(done)
		}()
		<-started
		setSize(t, wfs, fh, 0)
		setSize(t, wfs, fh, 5)
		close(release)
		<-done

		if err := fh.readError(); err != nil {
			t.Fatalf("filer hides %v: read error %v", filerHides, err)
		}
		if got := readHandle(t, fh, 8); got != "\x00\x00\x00\x00\x00" {
			t.Fatalf("filer hides %v: read %q after truncating to 0", filerHides, got)
		}
		if _, code := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: 5}, []byte("world")); code != fuse.OK {
			t.Fatalf("write: %v", code)
		}
		if got := readHandle(t, fh, 8); got != "world" {
			t.Fatalf("filer hides %v: read %q after writing", filerHides, got)
		}
	}
}

// A failed lookup of a hidden entry must not turn the next retry into a
// manifest resolve, which succeeds for an inline file and reads zeros.
func TestRefetchKeepsHiddenAfterLookupError(t *testing.T) {
	defer func() { lookupEntry = filer_pb.GetEntry }()
	unavailable := errors.New("rpc error: code = Unavailable")
	expire := func(fh *FileHandle) {
		f := *fh.readFailure.Load()
		f.at = time.Now().Add(-readRetryInterval)
		fh.readFailure.Store(&f)
	}
	read := func(fh *FileHandle) (string, error) {
		buf := make([]byte, 8)
		n, err := readDataByFileHandle(buf, fh, 0)
		return string(buf[:n]), err
	}

	for _, filerHides := range []bool{false, true} {
		wfs := testWFS(t)
		fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
		lookups := 0
		lookupEntry = func(filer_pb.FilerClient, util.FullPath) (*filer_pb.Entry, error) {
			lookups++
			switch {
			case lookups == 1:
				return nil, unavailable
			case filerHides:
				return hiddenEntry("a", 5), nil
			}
			return &filer_pb.Entry{Name: "a", Content: []byte("hello"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}, nil
		}
		if got, err := read(fh); err != unavailable {
			t.Fatalf("first read %q, %v", got, err)
		}
		expire(fh)
		got, err := read(fh)
		if lookups != 2 {
			t.Fatalf("filer hides %v: %d lookups, the retry did not ask the filer", filerHides, lookups)
		}
		if filerHides && (err != filer_pb.ErrContentHidden || got != "") {
			t.Fatalf("still hidden: read %q, %v", got, err)
		}
		if !filerHides && (err != nil || got != "hello") {
			t.Fatalf("renewed: read %q, %v", got, err)
		}
	}

	// A truncation to 0 after a failed lookup drops the hidden content and
	// the failure with it.
	wfs := testWFS(t)
	fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
	lookupEntry = func(filer_pb.FilerClient, util.FullPath) (*filer_pb.Entry, error) { return nil, unavailable }
	if _, err := read(fh); err != unavailable {
		t.Fatalf("read: %v", err)
	}
	setSize(t, wfs, fh, 0)
	if _, code := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: 5}, []byte("world")); code != fuse.OK {
		t.Fatalf("write: %v", code)
	}
	if got, err := read(fh); err != nil || got != "world" {
		t.Fatalf("read after truncate and write: %q, %v", got, err)
	}
}

// A chmod not flushed yet must not keep the chunk-less hidden entry when
// another client rewrote the file in chunks: reads would return zeros and a
// flush would drop the chunks.
func TestRefetchTakesChunksWithLocalChmod(t *testing.T) {
	defer func() { lookupEntry = filer_pb.GetEntry }()
	lookupEntry = func(filer_pb.FilerClient, util.FullPath) (*filer_pb.Entry, error) {
		return &filer_pb.Entry{Name: "a", Chunks: []*filer_pb.FileChunk{{FileId: "3,01", Size: 2000, ModifiedTsNs: 1}},
			Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 2000}}, nil
	}
	wfs := testWFS(t)
	fh := wfs.testOpen("/d/a", hiddenEntry("a", 5))
	in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: fuse.InHeader{NodeId: fh.inode}, Valid: fuse.FATTR_MODE | fuse.FATTR_FH, Fh: uint64(fh.fh), Mode: 0600}}
	if code := wfs.SetAttr(nil, in, &fuse.AttrOut{}); code != fuse.OK {
		t.Fatalf("chmod: %v", code)
	}
	fh.maybeRetryRead()
	if err := fh.readError(); err != nil {
		t.Fatalf("read error %v", err)
	}
	e := fh.entry.GetEntry()
	if len(e.Chunks) != 1 || e.Attributes.FileSize != 2000 || e.Attributes.FileMode&0777 != 0600 || e.ContentHidden {
		t.Fatalf("entry %+v", e)
	}
	if found, _ := fh.entryChunkGroup.SearchChunks(0, 2000, SEEK_DATA); !found {
		t.Fatal("the chunk group does not hold the filer's chunks")
	}
}
