package mount

import (
	"errors"
	"testing"
	"time"

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
