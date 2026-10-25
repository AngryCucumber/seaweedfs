package filer_test

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/filer/leveldb"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

// bootstrapPeer is a filer stub for the bootstrap: a namespace to copy,
// which may hide its inline content, a metadata log directory listing, and
// a subscription that replays recorded events.
type bootstrapPeer struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu       sync.Mutex
	hidden   bool
	entries  []*filer_pb.TraverseBfsMetadataResponse
	logDays  []string
	events   []*filer_pb.SubscribeMetadataResponse
	sinceNs  []int64
	bfsCalls atomic.Int32
}

func (p *bootstrapPeer) TraverseBfsMetadata(_ *filer_pb.TraverseBfsMetadataRequest, stream filer_pb.SeaweedFiler_TraverseBfsMetadataServer) error {
	p.bfsCalls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		resp := &filer_pb.TraverseBfsMetadataResponse{Directory: e.Directory, Entry: &filer_pb.Entry{Name: e.Entry.Name, IsDirectory: e.Entry.IsDirectory, Content: e.Entry.Content, Attributes: &filer_pb.FuseAttributes{FileMode: 0644}}}
		if p.hidden && len(e.Entry.Content) > 0 {
			resp.Entry.Content, resp.Entry.ContentHidden = nil, true
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (p *bootstrapPeer) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	if req.Directory != filer.SystemLogDir {
		return status.Errorf(codes.NotFound, "%s: %v", req.Directory, filer_pb.ErrNotFound)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, day := range p.logDays {
		if req.Limit > 0 && uint32(i) >= req.Limit {
			break
		}
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: &filer_pb.Entry{Name: day, IsDirectory: true}}); err != nil {
			return err
		}
	}
	return nil
}

// GetFilerConfiguration gives the peer's store signature, which the
// aggregator reads before following it.
func (p *bootstrapPeer) GetFilerConfiguration(context.Context, *filer_pb.GetFilerConfigurationRequest) (*filer_pb.GetFilerConfigurationResponse, error) {
	return &filer_pb.GetFilerConfigurationResponse{Signature: 424242}, nil
}

func (p *bootstrapPeer) SubscribeLocalMetadata(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeLocalMetadataServer) error {
	p.mu.Lock()
	p.sinceNs = append(p.sinceNs, req.SinceNs)
	events := append([]*filer_pb.SubscribeMetadataResponse{}, p.events...)
	p.mu.Unlock()
	for _, e := range events {
		if e.TsNs > req.SinceNs {
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
	<-stream.Context().Done()
	return nil
}

// testStoreDir configures a leveldb store in a directory.
type testStoreDir string

func (d testStoreDir) GetString(string) string                  { return string(d) }
func (d testStoreDir) GetBool(string) bool                      { return false }
func (d testStoreDir) GetInt(string) int                        { return 0 }
func (d testStoreDir) GetStringSlice(string) []string           { return nil }
func (d testStoreDir) SetDefault(key string, value interface{}) {}

func startBootstrapPeer(t *testing.T) (*bootstrapPeer, pb.ServerAddress) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &bootstrapPeer{}
	server := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(server, peer)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	port := listener.Addr().(*net.TCPAddr).Port
	return peer, pb.NewServerAddress("127.0.0.1", port-10000, port)
}

func newBootstrapFiler(t *testing.T, store filer.FilerStore) (*filer.Filer, bool) {
	t.Helper()
	f := filer.NewFiler(pb.ServerDiscovery{}, grpc.WithTransportCredentials(insecure.NewCredentials()), "127.0.0.1:8888", "", "", "", "", 255, nil)
	return f, f.SetStore(store)
}

// A fresh store keeps no signature until its bootstrap completes, remembers
// the bootstrap cut of an unfinished attempt, and the copy from the peer
// stops after the retry limit or when the master refuses this filer.
func TestBootstrapFreshStoreUntilFinished(t *testing.T) {
	savedInterval, savedLimit := filer.BootstrapRetryInterval, filer.BootstrapRetryLimit
	filer.BootstrapRetryInterval, filer.BootstrapRetryLimit = 20*time.Millisecond, 100*time.Millisecond
	defer func() { filer.BootstrapRetryInterval, filer.BootstrapRetryLimit = savedInterval, savedLimit }()
	store := &leveldb.LevelDBStore{}
	if err := store.Initialize(testStoreDir(t.TempDir()), ""); err != nil {
		t.Fatal(err)
	}
	peer, peerAddr := startBootstrapPeer(t)
	nodes := []*master_pb.ClusterNodeUpdate{{Address: string(peerAddr), CreatedAtNs: 1, IsAdd: true}}
	self := pb.ServerAddress("127.0.0.1:8888")
	ctx := context.Background()

	f, fresh := newBootstrapFiler(t, store)
	if !fresh {
		t.Fatal("new store is not fresh")
	}
	if _, err := store.KvGet(ctx, []byte(filer.FilerStoreId)); err != filer.ErrKvNotFound {
		t.Fatalf("signature written before the bootstrap: %v", err)
	}
	cut := time.Unix(0, time.Now().Add(-time.Hour).UnixNano())
	since, err := f.BootstrapSince(cut)
	if err != nil || !since.Equal(cut) {
		t.Fatalf("first cut %v %v", since, err)
	}

	// The peer hides content: retries, then gives up.
	peer.mu.Lock()
	peer.hidden = true
	peer.entries = []*filer_pb.TraverseBfsMetadataResponse{
		{Directory: "/", Entry: &filer_pb.Entry{Name: "d", IsDirectory: true}},
		{Directory: "/d", Entry: &filer_pb.Entry{Name: "x", Content: []byte("x")}},
	}
	peer.mu.Unlock()
	err = f.MaybeBootstrapFromOnePeer(ctx, self, nodes, since)
	if err == nil || !strings.Contains(err.Error(), "still hides content") {
		t.Fatalf("hidden peer: %v", err)
	}
	if peer.bfsCalls.Load() < 2 {
		t.Fatalf("%d attempts", peer.bfsCalls.Load())
	}

	// Refused by the master: stops at once with the cause.
	filer.BootstrapRetryLimit = time.Hour
	refused, abort := context.WithCancelCause(ctx)
	abort(errors.New("master refused this filer: NODE_LIMIT_EXCEEDED"))
	err = f.MaybeBootstrapFromOnePeer(refused, self, nodes, since)
	if err == nil || !strings.Contains(err.Error(), "NODE_LIMIT_EXCEEDED") {
		t.Fatalf("refused: %v", err)
	}

	// A restart finds the store fresh, with the same cut.
	f2, fresh := newBootstrapFiler(t, store)
	if !fresh {
		t.Fatal("store initialized although the bootstrap did not finish")
	}
	since2, err := f2.BootstrapSince(time.Now())
	if err != nil || !since2.Equal(cut) {
		t.Fatalf("resumed cut %v %v, want %v", since2, err, cut)
	}

	// The peer's log no longer reaches the cut: refused with the hint.
	peer.mu.Lock()
	peer.hidden = false
	peer.logDays = []string{since.UTC().Add(48 * time.Hour).Format("2006-01-02")}
	peer.mu.Unlock()
	err = f2.MaybeBootstrapFromOnePeer(ctx, self, nodes, since2)
	if err == nil || !strings.Contains(err.Error(), "empty this filer's store") {
		t.Fatalf("log gap: %v", err)
	}
	peer.mu.Lock()
	peer.logDays = []string{since.UTC().Format("2006-01-02"), since.UTC().Add(24 * time.Hour).Format("2006-01-02")}
	peer.mu.Unlock()
	if err := f2.MaybeBootstrapFromOnePeer(ctx, self, nodes, since2); err != nil {
		t.Fatal(err)
	}
	if e, err := f2.FindEntry(ctx, "/d/x"); err != nil || string(e.Content) != "x" {
		t.Fatalf("copied entry %+v %v", e, err)
	}
	if err := f2.FinishBootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.KvGet(ctx, []byte(filer.FilerBootstrapSince)); err != filer.ErrKvNotFound {
		t.Fatalf("cut kept after the bootstrap: %v", err)
	}
	f3, fresh := newBootstrapFiler(t, store)
	if fresh || f3.Signature != f2.Signature {
		t.Fatalf("after the bootstrap: fresh %v, signature %d want %d", fresh, f3.Signature, f2.Signature)
	}
}

// A file copied by a first attempt and deleted at the source before the
// second one is gone once the events from the cut are replayed: the
// subscriptions start from the persisted cut, not from the restart time.
func TestBootstrapReplaysFromCut(t *testing.T) {
	store := &leveldb.LevelDBStore{}
	if err := store.Initialize(testStoreDir(t.TempDir()), ""); err != nil {
		t.Fatal(err)
	}
	peer, peerAddr := startBootstrapPeer(t)
	nodes := []*master_pb.ClusterNodeUpdate{{Address: string(peerAddr), CreatedAtNs: 1, IsAdd: true}}
	self := pb.ServerAddress("127.0.0.1:8888")
	ctx := context.Background()
	cut := time.Unix(0, time.Now().Add(-time.Minute).UnixNano())

	// The first attempt copied /d/x and did not finish.
	f, _ := newBootstrapFiler(t, store)
	if _, err := f.BootstrapSince(cut); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.InsertEntry(ctx, &filer.Entry{FullPath: "/d", Attr: filer.Attr{Mode: 0755 | os.ModeDir}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.InsertEntry(ctx, &filer.Entry{FullPath: "/d/x", Attr: filer.Attr{Mode: 0644}, Content: []byte("x")}); err != nil {
		t.Fatal(err)
	}

	// Meanwhile the source deleted /d/x and created /d/y.
	peer.mu.Lock()
	peer.entries = []*filer_pb.TraverseBfsMetadataResponse{
		{Directory: "/", Entry: &filer_pb.Entry{Name: "d", IsDirectory: true}},
		{Directory: "/d", Entry: &filer_pb.Entry{Name: "y", Content: []byte("y")}},
	}
	peer.events = []*filer_pb.SubscribeMetadataResponse{
		{Directory: "/d", TsNs: cut.UnixNano() + 1000, EventNotification: &filer_pb.EventNotification{OldEntry: &filer_pb.Entry{Name: "x"}}},
	}
	peer.mu.Unlock()

	// The restart resumes the cut and completes the copy.
	f2, fresh := newBootstrapFiler(t, store)
	if !fresh {
		t.Fatal("store initialized although the bootstrap did not finish")
	}
	since, err := f2.BootstrapSince(time.Now())
	if err != nil || !since.Equal(cut) {
		t.Fatalf("cut %v %v", since, err)
	}
	if err := f2.MaybeBootstrapFromOnePeer(ctx, self, nodes, since); err != nil {
		t.Fatal(err)
	}
	if err := f2.FinishBootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.FindEntry(ctx, "/d/x"); err != nil {
		t.Fatalf("the copy alone does not remove /d/x: %v", err)
	}

	// The events from the cut delete it.
	f2.AggregateFromPeers(self, nodes, since)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := f2.FindEntry(ctx, "/d/x")
		if err == filer_pb.ErrNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/d/x still present: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	peer.mu.Lock()
	sinceNs := append([]int64{}, peer.sinceNs...)
	peer.mu.Unlock()
	if len(sinceNs) == 0 || sinceNs[0] != cut.UnixNano() {
		t.Fatalf("subscribed from %v, want the cut %d", sinceNs, cut.UnixNano())
	}
	if e, err := f2.FindEntry(ctx, "/d/y"); err != nil || string(e.Content) != "y" {
		t.Fatalf("/d/y %+v %v", e, err)
	}
}
