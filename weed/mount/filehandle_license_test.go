package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
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
func testWFS(t testing.TB) *WFS {
	mapper, err := meta_cache.NewUidGidMapper("", "")
	if err != nil {
		t.Fatal(err)
	}
	wfs := &WFS{
		option:      &Option{ChunkSizeLimit: 1 << 20, ConcurrentWriters: 4, uniqueCacheDirForWrite: t.TempDir(), UidGidMapper: mapper},
		inodeToPath: NewInodeToPath("/", 0),
		fhMap:       NewFileHandleToInode(),
		fhLockTable: util.NewLockTable[FileHandleId](),
	}
	// Metadata events reach handles as in a mount (invalidateHandle), but
	// only when a test delivers one.
	wfs.metaCache = meta_cache.NewMetaCache(t.TempDir(), mapper, "/", func(util.FullPath) {}, func(util.FullPath) bool { return true }, wfs.invalidateHandle)
	t.Cleanup(wfs.metaCache.Shutdown)
	return wfs
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

// A remote-only file is cached in the cluster on the first read, write or
// size change. Reads use the cached chunks, writes and size changes apply
// to them, and the filer's event for the download does not replace the
// handle.
func TestDownloadRemoteEntry(t *testing.T) {
	saved := cacheRemoteObject
	defer func() { cacheRemoteObject = saved }()
	remoteOnly := func() *filer_pb.Entry {
		return &filer_pb.Entry{Name: "a", RemoteEntry: &filer_pb.RemoteEntry{StorageName: "s3", RemoteSize: 5},
			Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}
	}
	var calls atomic.Int32
	cached := func() *filer_pb.Entry {
		e := remoteOnly()
		// The filer stamps cached chunks with the time of the download.
		e.Chunks = []*filer_pb.FileChunk{{FileId: "3,01637037d6", Size: 5, ModifiedTsNs: time.Now().UnixNano()}}
		e.RemoteEntry.LastLocalSyncTsNs = 1
		return e
	}
	cacheRemoteObject = func(*WFS, *filer_pb.CacheRemoteObjectToLocalClusterRequest) (*filer_pb.CacheRemoteObjectToLocalClusterResponse, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &filer_pb.CacheRemoteObjectToLocalClusterResponse{Entry: cached()}, nil
	}
	open := func() (*WFS, *FileHandle) {
		calls.Store(0)
		wfs := testWFS(t)
		wfs.chunkCache = chunk_cache.NewTieredChunkCache(16, t.TempDir(), 64, 1024)
		wfs.chunkCache.SetChunk("3,01637037d6", []byte("world"))
		return wfs, wfs.testOpen("/d/a", remoteOnly())
	}
	chmod := func(wfs *WFS, fh *FileHandle) {
		in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: fuse.InHeader{NodeId: fh.inode}, Valid: fuse.FATTR_MODE | fuse.FATTR_FH, Fh: uint64(fh.fh), Mode: 0600}}
		if code := wfs.SetAttr(nil, in, &fuse.AttrOut{}); code != fuse.OK {
			t.Fatalf("chmod: %v", code)
		}
	}
	write := func(wfs *WFS, fh *FileHandle, data string) {
		if _, code := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: uint32(len(data))}, []byte(data)); code != fuse.OK {
			t.Fatalf("write: %v", code)
		}
	}
	ownEvent := func(fh *FileHandle) bool {
		return fh.ownCacheEvent(cached())
	}

	// Concurrent first reads download once; later reads not at all.
	_, fh := open()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := readHandle(t, fh, 8); got != "world" {
				t.Errorf("read %q", got)
			}
		}()
	}
	wg.Wait()
	if got := readHandle(t, fh, 8); got != "world" || calls.Load() != 1 {
		t.Fatalf("read %q after %d downloads", got, calls.Load())
	}

	// A write and a chmod after the download survive the filer's event for
	// it, which is recognized once; an event with other chunks is not.
	wfs, fh := open()
	readHandle(t, fh, 8)
	write(wfs, fh, "newer")
	chmod(wfs, fh)
	other := cached()
	other.Chunks[0].FileId = "3,02637037d6"
	if fh.ownCacheEvent(other) || !ownEvent(fh) || ownEvent(fh) {
		t.Fatal("own download event not recognized exactly once")
	}
	if got := readHandle(t, fh, 8); got != "newer" || fh.entry.GetEntry().Attributes.FileMode&0777 != 0600 {
		t.Fatalf("read %q, mode %o", got, fh.entry.GetEntry().Attributes.FileMode)
	}

	// A write before any read caches the file first: the written data is
	// newer than the cached chunks, so a flush keeps it.
	wfs, fh = open()
	before := time.Now().UnixNano()
	write(wfs, fh, "newer")
	e := fh.entry.GetEntry()
	if calls.Load() != 1 || len(e.Chunks) != 1 || e.Chunks[0].ModifiedTsNs < before {
		t.Fatalf("write did not cache first: %d downloads, %+v", calls.Load(), e)
	}
	if got := readHandle(t, fh, 8); got != "newer" {
		t.Fatalf("read %q", got)
	}

	// A chmod before the download stays; the content comes from the filer.
	wfs, fh = open()
	chmod(wfs, fh)
	if got := readHandle(t, fh, 8); got != "world" || fh.entry.GetEntry().Attributes.FileMode&0777 != 0600 {
		t.Fatalf("read %q, mode %o", got, fh.entry.GetEntry().Attributes.FileMode)
	}
	if !ownEvent(fh) {
		t.Fatal("own download event not recognized")
	}

	// Truncating and growing cuts the cached chunks; truncating to 0
	// drops the remote object, so nothing is fetched back.
	wfs, fh = open()
	setSize(t, wfs, fh, 3)
	setSize(t, wfs, fh, 5)
	if got := readHandle(t, fh, 8); got != "wor\x00\x00" || calls.Load() != 1 {
		t.Fatalf("5->3->5: read %q after %d downloads", got, calls.Load())
	}
	wfs, fh = open()
	setSize(t, wfs, fh, 0)
	setSize(t, wfs, fh, 5)
	if got := readHandle(t, fh, 8); got != "\x00\x00\x00\x00\x00" || calls.Load() != 0 || fh.entry.GetEntry().RemoteEntry != nil {
		t.Fatalf("5->0->5: read %q after %d downloads", got, calls.Load())
	}

	// Without an open handle a size change caches the file into the entry.
	wfs, _ = open()
	entry := remoteOnly()
	if err := wfs.cacheRemoteEntry("/d/a", entry); err != nil || len(entry.Chunks) != 1 || entry.RemoteEntry.GetLastLocalSyncTsNs() != 1 {
		t.Fatalf("cache without a handle: %v %+v", err, entry)
	}

	// The filer found no remote object any more: no entry to install.
	cacheRemoteObject = func(*WFS, *filer_pb.CacheRemoteObjectToLocalClusterRequest) (*filer_pb.CacheRemoteObjectToLocalClusterResponse, error) {
		return &filer_pb.CacheRemoteObjectToLocalClusterResponse{}, nil
	}
	_, fh = open()
	if _, err := readDataByFileHandle(make([]byte, 8), fh, 0); err == nil || fh.entry.GetEntry() == nil {
		t.Fatalf("read without a cached entry: %v", err)
	}
}

// remoteOnlyEntry is a file whose content is only in remote storage;
// cachedRemoteEntry is the same file after the filer cached it in the
// cluster, its chunk stamped with tsNs by the filer's clock.
func remoteOnlyEntry() *filer_pb.Entry {
	return &filer_pb.Entry{Name: "a", RemoteEntry: &filer_pb.RemoteEntry{StorageName: "s3", RemoteSize: 5},
		Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}}
}

func cachedRemoteEntry(tsNs int64) *filer_pb.Entry {
	e := remoteOnlyEntry()
	e.Chunks = []*filer_pb.FileChunk{{FileId: "3,01637037d6", Size: 5, ModifiedTsNs: tsNs}}
	e.RemoteEntry.LastLocalSyncTsNs = 1
	return e
}

// flushFiler is a filer stub for flushes: it assigns file ids and records
// the entry it is given.
type flushFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	volumeURL string
	mu        sync.Mutex
	fileKey   uint64
	saved     *filer_pb.Entry
}

func (s *flushFiler) AssignVolume(context.Context, *filer_pb.AssignVolumeRequest) (*filer_pb.AssignVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileKey++
	return &filer_pb.AssignVolumeResponse{FileId: fmt.Sprintf("3,%02x637037d6", s.fileKey), Location: &filer_pb.Location{Url: s.volumeURL}}, nil
}

func (s *flushFiler) CreateEntry(_ context.Context, r *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = proto.Clone(r.Entry).(*filer_pb.Entry)
	return &filer_pb.CreateEntryResponse{}, nil
}

func (s *flushFiler) entry() *filer_pb.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

// testFlushWFS is a mount whose flushes reach flushFiler; uploads go to an
// HTTP stub that reports 5 bytes stored.
func testFlushWFS(t *testing.T) (*WFS, *flushFiler) {
	t.Helper()
	util_http.InitGlobalHttpClient()
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"a","size":5}`)
	}))
	t.Cleanup(upload.Close)
	// File ids start at 3,02...: 3,01... is the chunk of cachedRemoteEntry.
	stub := &flushFiler{volumeURL: strings.TrimPrefix(upload.URL, "http://"), fileKey: 1}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(server, stub)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	mapper, err := meta_cache.NewUidGidMapper("", "")
	if err != nil {
		t.Fatal(err)
	}
	_, httpPort, _ := net.SplitHostPort(stub.volumeURL)
	port, _ := strconv.Atoi(httpPort)
	wfs := NewSeaweedFileSystem(&Option{
		FilerAddresses:     []pb.ServerAddress{pb.NewServerAddress("127.0.0.1", port, listener.Addr().(*net.TCPAddr).Port)},
		GrpcDialOption:     grpc.WithTransportCredentials(insecure.NewCredentials()),
		FilerMountRootPath: "/", CacheDirForRead: t.TempDir(), CacheDirForWrite: t.TempDir(), CacheSizeMBForRead: 64,
		ChunkSizeLimit: 1 << 20, ConcurrentWriters: 4, UidGidMapper: mapper,
	})
	t.Cleanup(wfs.metaCache.Shutdown)
	wfs.inodeToPath.Lookup("/d", time.Now().Unix(), true, false, 0, true)
	wfs.inodeToPath.MarkChildrenCached("/d")
	return wfs, stub
}

// A write is newer than every chunk of the entry even when the filer's
// clock, which stamped the chunks it cached, is ahead of the mount's:
// overlapping chunks are resolved by timestamp when the handle flushes.
// Later writes stay newer than earlier ones.
func TestWriteNewerThanFilerChunks(t *testing.T) {
	saved := cacheRemoteObject
	defer func() { cacheRemoteObject = saved }()
	ahead := time.Now().Add(5 * time.Second).UnixNano()
	cacheRemoteObject = func(*WFS, *filer_pb.CacheRemoteObjectToLocalClusterRequest) (*filer_pb.CacheRemoteObjectToLocalClusterResponse, error) {
		return &filer_pb.CacheRemoteObjectToLocalClusterResponse{Entry: cachedRemoteEntry(ahead)}, nil
	}
	wfs, stub := testFlushWFS(t)
	fh := wfs.testOpen("/d/a", remoteOnlyEntry())
	write := func(data string) {
		if _, code := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: uint32(len(data))}, []byte(data)); code != fuse.OK {
			t.Fatalf("write %q: %v", data, code)
		}
	}

	write("newer")
	first := fh.lastWriteTsNs
	if first <= ahead || len(fh.entry.GetEntry().Chunks) != 1 {
		t.Fatalf("write at %d over a chunk cached at %d, entry %+v", first, ahead, fh.entry.GetEntry())
	}
	if code := wfs.Flush(nil, &fuse.FlushIn{Fh: uint64(fh.fh)}); code != fuse.OK {
		t.Fatalf("flush: %v", code)
	}
	// The written chunk replaced the cached one in the filer and in the handle.
	stored := stub.entry()
	if len(stored.Chunks) != 1 || stored.Chunks[0].GetFileIdString() != "3,02637037d6" || stored.Chunks[0].ModifiedTsNs != first {
		t.Fatalf("flushed chunks %v, want the write at %d", stored.Chunks, first)
	}
	if got := readHandle(t, fh, 8); got != "newer" {
		t.Fatalf("read %q after the flush", got)
	}

	write("newest")
	if fh.lastWriteTsNs <= first {
		t.Fatalf("second write at %d after the first at %d", fh.lastWriteTsNs, first)
	}

	// A copy into the file is a write too.
	src := wfs.testOpen("/d/src", &filer_pb.Entry{Name: "src", Content: []byte("world"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644, FileSize: 5}})
	second := fh.lastWriteTsNs
	if written, code := wfs.CopyFileRange(nil, &fuse.CopyFileRangeIn{FhIn: uint64(src.fh), FhOut: uint64(fh.fh), Len: 5}); code != fuse.OK || written != 5 {
		t.Fatalf("copy: %v %d", code, written)
	}
	if fh.lastWriteTsNs <= second {
		t.Fatalf("copy at %d after the write at %d", fh.lastWriteTsNs, second)
	}
}

// Chunks of finished uploads are appended to the entry under its own lock
// while writes run: a write does not read the chunks, and is later than
// the appended ones.
func TestWriteAfterUploadedChunks(t *testing.T) {
	wfs := testWFS(t)
	fh := wfs.testOpen("/d/a", &filer_pb.Entry{Name: "a", Attributes: &filer_pb.FuseAttributes{FileMode: 0644}})
	ahead := time.Now().Add(5 * time.Second).UnixNano()
	const uploads = 1000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < uploads; i++ {
			chunk := &filer_pb.FileChunk{FileId: fmt.Sprintf("3,%x637037d6", i+1), Offset: int64(i) * 4096, Size: 4096, ModifiedTsNs: ahead + int64(i)}
			fh.AddChunks([]*filer_pb.FileChunk{chunk})
			fh.entryChunkGroup.AddChunk(chunk)
		}
	}()
	for i := 0; i < uploads; i++ {
		lock := wfs.fhLockTable.AcquireLock("Write", fh.fh, util.ExclusiveLock)
		fh.writeTsNs()
		if err := fh.cacheRemoteBeforeChange(); err != nil {
			t.Error(err)
		}
		wfs.fhLockTable.ReleaseLock(fh.fh, lock)
	}
	wg.Wait()
	if _, code := wfs.Write(nil, &fuse.WriteIn{Fh: uint64(fh.fh), Size: 5}, []byte("newer")); code != fuse.OK {
		t.Fatalf("write: %v", code)
	}
	if fh.lastWriteTsNs <= ahead+uploads-1 {
		t.Fatalf("write at %d, uploaded chunks up to %d", fh.lastWriteTsNs, ahead+uploads-1)
	}
}

// The write timestamp does not depend on the number of chunks.
func BenchmarkWriteTsNs(b *testing.B) {
	for _, n := range []int{500, 50000} {
		b.Run(fmt.Sprintf("chunks_%d", n), func(b *testing.B) {
			wfs := testWFS(b)
			entry := &filer_pb.Entry{Name: "a", Attributes: &filer_pb.FuseAttributes{FileMode: 0644}}
			for i := 0; i < n; i++ {
				entry.Chunks = append(entry.Chunks, &filer_pb.FileChunk{FileId: fmt.Sprintf("3,%x637037d6", i+1), Offset: int64(i) * 4096, Size: 4096, ModifiedTsNs: int64(i)})
			}
			fh := wfs.testOpen("/d/a", entry)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				fh.writeTsNs()
			}
		})
	}
}

// Attribute changes on a handle are ordered with its reads, writes and the
// filer's metadata events, like writes: a download running for a read
// completes before a truncation, and the filer's event for a download made
// by a truncation waits for it.
func TestSetAttrOrderedWithDownload(t *testing.T) {
	saved := cacheRemoteObject
	defer func() { cacheRemoteObject = saved }()
	open := func() (*WFS, *FileHandle) {
		wfs := testWFS(t)
		wfs.chunkCache = chunk_cache.NewTieredChunkCache(16, t.TempDir(), 64, 1024)
		wfs.chunkCache.SetChunk("3,01637037d6", []byte("world"))
		return wfs, wfs.testOpen("/d/a", remoteOnlyEntry())
	}
	setAttr := func(wfs *WFS, fh *FileHandle, in *fuse.SetAttrIn) fuse.Status {
		in.NodeId, in.Valid, in.Fh = fh.inode, in.Valid|fuse.FATTR_FH, uint64(fh.fh)
		return wfs.SetAttr(nil, in, &fuse.AttrOut{})
	}

	// A chmod, then a truncation whose download the filer announces before
	// it responds: the event waits for the truncation, is recognized as the
	// handle's own, and changes nothing.
	wfs, fh := open()
	if code := setAttr(wfs, fh, &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{Valid: fuse.FATTR_MODE, Mode: 0600}}); code != fuse.OK {
		t.Fatalf("chmod: %v", code)
	}
	var event sync.WaitGroup
	cacheRemoteObject = func(*WFS, *filer_pb.CacheRemoteObjectToLocalClusterRequest) (*filer_pb.CacheRemoteObjectToLocalClusterResponse, error) {
		wfs.metaCache.InsertEntry(context.Background(), wfs.localEntryFromFiler("/d", cachedRemoteEntry(1)))
		event.Add(1)
		go func() {
			defer event.Done()
			wfs.invalidateHandle("/d/a", remoteOnlyEntry())
		}()
		time.Sleep(50 * time.Millisecond)
		return &filer_pb.CacheRemoteObjectToLocalClusterResponse{Entry: cachedRemoteEntry(1)}, nil
	}
	setSize(t, wfs, fh, 3)
	event.Wait()
	e := fh.entry.GetEntry()
	if got := readHandle(t, fh, 8); got != "wor" || e.Attributes.FileMode&0777 != 0600 || e.Attributes.FileSize != 3 || fh.cachedRemote.Load() != nil {
		t.Fatalf("after the event: read %q, mode %o, size %d, download recorded %v", got, e.Attributes.FileMode, e.Attributes.FileSize, fh.cachedRemote.Load() != nil)
	}

	// A truncation to 0 during the download for a read waits for it, then
	// cuts the cached file; the event of the download changes nothing.
	wfs, fh = open()
	entered, release := make(chan struct{}), make(chan struct{})
	cacheRemoteObject = func(*WFS, *filer_pb.CacheRemoteObjectToLocalClusterRequest) (*filer_pb.CacheRemoteObjectToLocalClusterResponse, error) {
		close(entered)
		<-release
		return &filer_pb.CacheRemoteObjectToLocalClusterResponse{Entry: cachedRemoteEntry(1)}, nil
	}
	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 8)
		result, code := wfs.Read(nil, &fuse.ReadIn{Fh: uint64(fh.fh), Size: 8}, buf)
		if code != fuse.OK {
			read <- code.String()
			return
		}
		data, _ := result.Bytes(nil)
		read <- string(data)
	}()
	<-entered
	truncated := make(chan fuse.Status, 1)
	go func() {
		truncated <- setAttr(wfs, fh, &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{Valid: fuse.FATTR_SIZE, Size: 0}})
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case code := <-truncated:
		t.Fatalf("truncated during the download: %v", code)
	default:
	}
	close(release)
	if got := <-read; got != "world" {
		t.Fatalf("read %q", got)
	}
	if code := <-truncated; code != fuse.OK {
		t.Fatalf("truncate: %v", code)
	}
	wfs.invalidateHandle("/d/a", remoteOnlyEntry())
	e = fh.entry.GetEntry()
	if got := readHandle(t, fh, 8); got != "" || e.Attributes.FileSize != 0 || e.RemoteEntry != nil || len(e.Chunks) != 0 {
		t.Fatalf("after the event: read %q, entry %+v", got, e)
	}
}
