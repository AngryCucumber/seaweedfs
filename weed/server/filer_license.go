package weed_server

import (
	"bytes"
	"context"
	"net/http"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/remote_storage"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// licenseKvMaxOffsetBytes is the size of the sync offsets KvGet still
// returns while the license denies reads.
const licenseKvMaxOffsetBytes = 8

// licenseKvOffsetPrefixes are the KvGet keys of filer.sync, remote sync and
// peer metadata offsets (contract 5.3).
var licenseKvOffsetPrefixes = [][]byte{
	[]byte(filer.MetaOffsetPrefix),
	[]byte("sync."),
	[]byte(remote_storage.SyncKeyPrefix),
}

// licenseContentExempt reports whether the inline content of dir/name is
// configuration that S3, IAM, filers, mount and MQ read over gRPC; it is
// never hidden (contract 5.3). Lookups resolve "." and ".." in the joined
// path (util.JoinPath), so the decision uses the resolved path of the
// entry: dir "/etc/../data" or name "../data/a" under "/etc" is /data.
func licenseContentExempt(dir, name string) bool {
	full := path.Clean(dir + "/" + name)
	dir, name = path.Dir(full), path.Base(full)
	if dir == "/etc" || strings.HasPrefix(dir, filer.DirectoryEtcRoot) {
		return true
	}
	if dir == filer.TopicsDir || strings.HasPrefix(dir, filer.TopicsDir+"/") {
		return name == filer.TopicConfFile || strings.HasSuffix(name, ".offset")
	}
	return false
}

// licenseContentFilter hides inline file content in the entries one gRPC
// call sends while the license denies reads (contract 5.3, G1). Filers
// calling each other present the internal read token and see everything.
// The token is looked up once, the first time it matters.
type licenseContentFilter struct {
	ctx      context.Context
	node     *license.Node
	checked  bool
	internal bool
}

func (fs *FilerServer) newLicenseContentFilter(ctx context.Context) *licenseContentFilter {
	return &licenseContentFilter{ctx: ctx, node: fs.license}
}

// apply hides the inline content of e, sent as an entry of directory dir,
// if reads are denied now. The file size stays visible.
func (f *licenseContentFilter) apply(dir string, e *filer_pb.Entry) {
	if e == nil || len(e.Content) == 0 {
		return
	}
	ok, snap := f.node.Guard().AllowRead()
	if ok {
		return
	}
	if !f.checked {
		f.internal = licenseGrpcTokenValid(f.node, f.ctx)
		f.checked = true
	}
	if f.internal || licenseContentExempt(dir, e.Name) {
		return
	}
	if e.Attributes == nil {
		e.Attributes = &filer_pb.FuseAttributes{}
	}
	if e.Attributes.FileSize == 0 {
		e.Attributes.FileSize = uint64(len(e.Content))
	}
	e.Content = nil
	e.ContentHidden = true
	licenseNoteDenied(f.node, snap, "filer", licensePathFilerGrpc)
}

// applyEvent hides the inline content of both entries of a metadata event.
func (f *licenseContentFilter) applyEvent(dir string, event *filer_pb.EventNotification) {
	if event == nil {
		return
	}
	f.apply(dir, event.OldEntry)
	if event.NewParentPath != "" {
		f.apply(event.NewParentPath, event.NewEntry)
	} else {
		f.apply(dir, event.NewEntry)
	}
}

// licenseHideEntry returns entry for an HTTP metadata response (M1), or a
// copy without its inline content when the license denies reads.
func (fs *FilerServer) licenseHideEntry(entry *filer.Entry) (*filer.Entry, bool) {
	if len(entry.Content) == 0 {
		return entry, false
	}
	if ok, _ := fs.license.Guard().AllowRead(); ok {
		return entry, false
	}
	if dir, name := entry.FullPath.DirAndName(); licenseContentExempt(dir, name) {
		return entry, false
	}
	hidden := *entry
	if hidden.FileSize == 0 {
		hidden.FileSize = uint64(len(entry.Content))
	}
	hidden.Content = nil
	hidden.ContentHidden = true
	return &hidden, true
}

// licenseHideEntries is licenseHideEntry for a directory listing; it
// copies the slice only when an entry is hidden.
func (fs *FilerServer) licenseHideEntries(entries []*filer.Entry) ([]*filer.Entry, bool) {
	var out []*filer.Entry
	for i, entry := range entries {
		hidden, ok := fs.licenseHideEntry(entry)
		if !ok {
			if out != nil {
				out = append(out, entry)
			}
			continue
		}
		if out == nil {
			out = append(make([]*filer.Entry, 0, len(entries)), entries[:i]...)
		}
		out = append(out, hidden)
	}
	if out == nil {
		return entries, false
	}
	return out, true
}

func setContentHiddenHeader(w http.ResponseWriter, hidden bool) {
	if hidden {
		w.Header().Set(license.ContentHiddenHeader, "true")
	}
}

// licenseMergeHiddenContent applies the write rules of contract 5.3 to an
// incoming entry that has content_hidden set. existing may be nil; it is
// then looked up.
func (fs *FilerServer) licenseMergeHiddenContent(ctx context.Context, fullpath util.FullPath, existing *filer.Entry, in *filer_pb.Entry) error {
	if existing == nil && len(in.Content) == 0 {
		found, err := fs.filer.FindEntry(ctx, fullpath)
		if err != nil && err != filer_pb.ErrNotFound {
			return err
		}
		existing = found
		if existing == nil && len(in.HardLinkId) > 0 {
			// A new name of a hard link shares the data of its other names.
			if existing, err = fs.hardLinkEntry(ctx, in.HardLinkId); err != nil {
				return err
			}
		}
	}
	return mergeHiddenContent(existing, in)
}

// hardLinkEntry returns the shared data of a hard link, or nil.
func (fs *FilerServer) hardLinkEntry(ctx context.Context, id []byte) (*filer.Entry, error) {
	blob, err := fs.filer.Store.KvGet(ctx, id)
	if err == filer.ErrKvNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entry := &filer.Entry{}
	if err := entry.DecodeAttributesAndChunks(blob); err != nil {
		return nil, err
	}
	return entry, nil
}

// mergeHiddenContent: a metadata-only update keeps the stored inline
// content, an entry whose bytes cannot be known is refused with
// CONTENT_HIDDEN, and anything else (new content, changed chunks, a
// truncation) is written as sent. The flag itself is never stored.
func mergeHiddenContent(existing *filer.Entry, in *filer_pb.Entry) error {
	in.ContentHidden = false
	if len(in.Content) > 0 {
		return nil
	}
	if existing != nil && filer.FileSize(in) == existing.Size() && sameChunks(existing.GetChunks(), in.GetChunks()) {
		in.Content = existing.Content
		return nil
	}
	if len(in.GetChunks()) == 0 && filer.FileSize(in) > 0 {
		return status.Error(codes.FailedPrecondition, "license: "+string(license.CodeContentHidden))
	}
	return nil
}

func sameChunks(a, b []*filer_pb.FileChunk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// licenseKvGetCheck decides a KvGet while the license denies reads. Hard
// link blobs hold whole entries with their content, so only the small sync
// offsets stay readable (contract 5.3): with offsetOnly set, the caller
// must also refuse values longer than licenseKvMaxOffsetBytes.
func (fs *FilerServer) licenseKvGetCheck(ctx context.Context, key []byte) (offsetOnly bool, err error) {
	ok, snap := fs.license.Guard().AllowRead()
	if ok || licenseGrpcTokenValid(fs.license, ctx) {
		return false, nil
	}
	for _, prefix := range licenseKvOffsetPrefixes {
		if bytes.HasPrefix(key, prefix) {
			return true, nil
		}
	}
	return false, fs.licenseKvDenied(snap)
}

func (fs *FilerServer) licenseKvDenied(snap *license.Snapshot) error {
	licenseNoteDenied(fs.license, snap, "filer", licensePathFilerGrpc)
	return status.Error(codes.PermissionDenied, "license: "+string(snap.Reason(fs.license.Guard().Now())))
}
