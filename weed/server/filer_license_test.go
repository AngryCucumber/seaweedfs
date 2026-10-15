package weed_server

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/filer/leveldb"
	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/license/licensetest"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func inlineEntry(name, content string) *filer_pb.Entry {
	return &filer_pb.Entry{Name: name, Content: []byte(content), Attributes: &filer_pb.FuseAttributes{}}
}

func TestLicenseContentExempt(t *testing.T) {
	exempt := [][2]string{
		{"/etc/iam", "identity.json"},
		{"/etc", "anything"},
		{"/etc/seaweedfs", "filer.conf"},
		{"/etc/remote", "mount.mapping"},
		{"/topics/ns/t", "topic.conf"},
		{"/topics/ns/t/v2025", "group.offset"},
		{"/etc/", "a"},
		{"/data/../etc/iam", "identity.json"},
		{"/data", "../etc/iam/identity.json"},
	}
	for _, p := range exempt {
		if !licenseContentExempt(p[0], p[1]) {
			t.Errorf("%s/%s should be exempt", p[0], p[1])
		}
	}
	hidden := [][2]string{
		{"/", "etc"},
		{"/etcx", "a"},
		{"/buckets/b", "topic.conf"},
		{"/topics/ns/t", "data"},
		{"/buckets/etc", "identity.json"},
		{"/etc/../data", "a"},
		{"/etc", "../data/a"},
		{"/etc/iam", "../../data/a"},
		{"/etc/../../data", "a"},
		{"/topics/../data", "topic.conf"},
		{"etc/iam", "identity.json"},
	}
	for _, p := range hidden {
		if licenseContentExempt(p[0], p[1]) {
			t.Errorf("%s/%s should not be exempt", p[0], p[1])
		}
	}
}

func TestLicenseContentFilter(t *testing.T) {
	denied := &FilerServer{license: licensetest.DeniedNode(t)}
	valid := &FilerServer{license: licensetest.ValidNode(t)}

	e := inlineEntry("a.txt", "secret")
	valid.newLicenseContentFilter(context.Background()).apply("/data", e)
	if string(e.Content) != "secret" || e.ContentHidden {
		t.Fatalf("valid license changed %+v", e)
	}

	e = inlineEntry("a.txt", "secret")
	denied.newLicenseContentFilter(context.Background()).apply("/data", e)
	if e.Content != nil || !e.ContentHidden || e.Attributes.FileSize != 6 {
		t.Fatalf("not hidden: %+v", e)
	}

	e = inlineEntry("a.txt", "secret")
	e.Attributes.FileSize = 9
	denied.newLicenseContentFilter(context.Background()).apply("/data", e)
	if e.Attributes.FileSize != 9 {
		t.Fatalf("file size changed to %d", e.Attributes.FileSize)
	}

	e = inlineEntry("identity.json", "{}")
	denied.newLicenseContentFilter(context.Background()).apply("/etc/iam", e)
	if string(e.Content) != "{}" || e.ContentHidden {
		t.Fatalf("exempt entry hidden: %+v", e)
	}

	// A request for /etc/../data/a reads /data/a: not exempt.
	e = inlineEntry("a", "secret")
	denied.newLicenseContentFilter(context.Background()).apply("/etc/../data", e)
	if e.Content != nil || !e.ContentHidden {
		t.Fatalf("dot-dot path bypassed hiding: %+v", e)
	}

	internal := metadata.NewIncomingContext(context.Background(), metadata.Pairs(license.InternalReadMetadataKey, licensetest.Token()))
	f := denied.newLicenseContentFilter(internal)
	for i := 0; i < 2; i++ {
		e = inlineEntry("a.txt", "secret")
		f.apply("/data", e)
		if string(e.Content) != "secret" {
			t.Fatalf("filer with token got hidden content")
		}
	}

	event := &filer_pb.EventNotification{OldEntry: inlineEntry("a", "old"), NewEntry: inlineEntry("b", "new"), NewParentPath: "/etc/s3"}
	denied.newLicenseContentFilter(context.Background()).applyEvent("/data", event)
	if !event.OldEntry.ContentHidden || event.NewEntry.ContentHidden {
		t.Fatalf("event: old hidden %v, new (renamed into /etc) hidden %v", event.OldEntry.ContentHidden, event.NewEntry.ContentHidden)
	}
	if !filer_pb.EventHasHiddenContent(event) {
		t.Fatal("EventHasHiddenContent")
	}
}

func TestLicenseHideEntries(t *testing.T) {
	denied := &FilerServer{license: licensetest.DeniedNode(t)}
	chunked := &filer.Entry{FullPath: "/d/big", Chunks: []*filer_pb.FileChunk{{FileId: "3,01"}}}
	inline := &filer.Entry{FullPath: "/d/small", Content: []byte("abc")}
	conf := &filer.Entry{FullPath: "/etc/seaweedfs/filer.conf", Content: []byte("x")}

	entries := []*filer.Entry{chunked, inline, conf}
	out, hidden := denied.licenseHideEntries(entries)
	if !hidden || out[0] != chunked || out[2] != conf || out[1] == inline {
		t.Fatalf("got %v %v", out, hidden)
	}
	if out[1].Content != nil || !out[1].ContentHidden || out[1].FileSize != 3 || out[1].FullPath != inline.FullPath {
		t.Fatalf("hidden copy %+v", out[1])
	}
	if string(inline.Content) != "abc" || inline.ContentHidden {
		t.Fatal("the stored entry was changed")
	}

	valid := &FilerServer{license: licensetest.ValidNode(t)}
	if out, hidden := valid.licenseHideEntries(entries); hidden || &out[0] != &entries[0] {
		t.Fatal("valid license copied the listing")
	}
}

func TestMergeHiddenContent(t *testing.T) {
	chunk := &filer_pb.FileChunk{FileId: "3,01", Size: 10}
	existing := &filer.Entry{FullPath: "/d/f", Content: []byte("hello"), Attr: filer.Attr{FileSize: 5}}
	hidden := func(size uint64, chunks ...*filer_pb.FileChunk) *filer_pb.Entry {
		return &filer_pb.Entry{Name: "f", ContentHidden: true, Chunks: chunks, Attributes: &filer_pb.FuseAttributes{FileSize: size}}
	}

	// Metadata-only update, such as S3 tagging or chmod: keep the content.
	in := hidden(5)
	if err := mergeHiddenContent(existing, in); err != nil || string(in.Content) != "hello" || in.ContentHidden {
		t.Fatalf("metadata update: %v %+v", err, in)
	}

	// New content sent explicitly.
	in = hidden(3)
	in.Content = []byte("new")
	if err := mergeHiddenContent(existing, in); err != nil || string(in.Content) != "new" || in.ContentHidden {
		t.Fatalf("explicit content: %v %+v", err, in)
	}

	// Copied to a new place or changed: the bytes are unknown.
	for _, c := range []struct {
		existing *filer.Entry
		in       *filer_pb.Entry
	}{{nil, hidden(5)}, {existing, hidden(6)}} {
		err := mergeHiddenContent(c.existing, c.in)
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "license: CONTENT_HIDDEN" {
			t.Fatalf("unknown bytes: %v", err)
		}
	}

	// Data rewritten by chunks or truncated: written as sent.
	in = hidden(10, chunk)
	if err := mergeHiddenContent(existing, in); err != nil || in.Content != nil || in.ContentHidden {
		t.Fatalf("rewritten: %v %+v", err, in)
	}
	in = hidden(0)
	if err := mergeHiddenContent(existing, in); err != nil || in.Content != nil || in.ContentHidden {
		t.Fatalf("truncated: %v %+v", err, in)
	}

	// The stored form never carries the flag.
	stored := filer.FromPbEntry("/d", hidden(5))
	stored.ContentHidden = false
	if stored.ToProtoEntry().ContentHidden {
		t.Fatal("flag survived")
	}
}

func TestLicenseKvGetCheck(t *testing.T) {
	denied := &FilerServer{license: licensetest.DeniedNode(t)}
	for _, key := range []string{"Meta\x00\x00\x00\x01", "sync.abc", "remote.sync.xyz"} {
		if offsetOnly, err := denied.licenseKvGetCheck(context.Background(), []byte(key)); err != nil || !offsetOnly {
			t.Errorf("%q: %v %v", key, offsetOnly, err)
		}
	}
	if _, err := denied.licenseKvGetCheck(context.Background(), util.RandomBytes(16)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("hard link id: %v", err)
	}
	internal := metadata.NewIncomingContext(context.Background(), metadata.Pairs(license.InternalReadMetadataKey, licensetest.Token()))
	if offsetOnly, err := denied.licenseKvGetCheck(internal, []byte("hardlink")); err != nil || offsetOnly {
		t.Errorf("internal: %v %v", offsetOnly, err)
	}
	valid := &FilerServer{license: licensetest.ValidNode(t)}
	if offsetOnly, err := valid.licenseKvGetCheck(context.Background(), []byte("hardlink")); err != nil || offsetOnly {
		t.Errorf("valid: %v %v", offsetOnly, err)
	}
}

type testStoreDir string

func (d testStoreDir) GetString(string) string                  { return string(d) }
func (d testStoreDir) GetBool(string) bool                      { return false }
func (d testStoreDir) GetInt(string) int                        { return 0 }
func (d testStoreDir) GetStringSlice(string) []string           { return nil }
func (d testStoreDir) SetDefault(key string, value interface{}) {}

func TestLicenseMergeHiddenHardLink(t *testing.T) {
	testFiler := filer.NewFiler(pb.ServerDiscovery{}, nil, "", "", "", "", "", 255, nil)
	store := &leveldb.LevelDBStore{}
	if err := store.Initialize(testStoreDir(t.TempDir()), ""); err != nil {
		t.Fatal(err)
	}
	testFiler.SetStore(store)
	fs := &FilerServer{filer: testFiler, license: licensetest.DeniedNode(t)}
	ctx := context.Background()

	linkId := filer.NewHardLinkId()
	original := &filer.Entry{FullPath: "/d/a", Attr: filer.Attr{Mode: 0644, FileSize: 5}, Content: []byte("hello"), HardLinkId: linkId, HardLinkCounter: 2}
	if err := testFiler.CreateEntry(ctx, original, false, false, nil, false, 255); err != nil {
		t.Fatal(err)
	}

	// mount creating the second name of a hidden inline file
	in := &filer_pb.Entry{Name: "b", ContentHidden: true, HardLinkId: linkId, HardLinkCounter: 2, Attributes: &filer_pb.FuseAttributes{FileSize: 5, FileMode: 0644}}
	if err := fs.licenseMergeHiddenContent(ctx, "/d/b", nil, in); err != nil || string(in.Content) != "hello" {
		t.Fatalf("hard link name: %v %q", err, in.Content)
	}

	// Without a hard link the bytes of a new name are unknown.
	in = &filer_pb.Entry{Name: "c", ContentHidden: true, Attributes: &filer_pb.FuseAttributes{FileSize: 5}}
	if err := fs.licenseMergeHiddenContent(ctx, "/d/c", nil, in); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("new name: %v", err)
	}
}
