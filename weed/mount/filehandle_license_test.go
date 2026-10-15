package mount

import (
	"errors"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
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
