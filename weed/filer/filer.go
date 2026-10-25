package filer

import (
	"context"
	"errors"
	"fmt"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3bucket"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/cluster/lock_manager"

	"github.com/seaweedfs/seaweedfs/weed/cluster"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"

	"google.golang.org/grpc"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

const (
	LogFlushInterval = time.Minute
	PaginationSize   = 1024
	FilerStoreId     = "filer.store.id"
)

var (
	OS_UID = uint32(os.Getuid())
	OS_GID = uint32(os.Getgid())
)

type Filer struct {
	UniqueFilerId       int32
	UniqueFilerEpoch    int32
	Store               VirtualFilerStore
	MasterClient        *wdclient.MasterClient
	fileIdDeletionQueue *util.UnboundedQueue
	GrpcDialOption      grpc.DialOption
	DirBucketsPath      string
	Cipher              bool
	LocalMetaLogBuffer  *log_buffer.LogBuffer
	metaLogCollection   string
	metaLogReplication  string
	MetaAggregator      *MetaAggregator
	Signature           int32
	FilerConf           *FilerConf
	RemoteStorage       *FilerRemoteStorage
	Dlm                 *lock_manager.DistributedLockManager
	MaxFilenameLength   uint32
}

func NewFiler(masters pb.ServerDiscovery, grpcDialOption grpc.DialOption, filerHost pb.ServerAddress, filerGroup string, collection string, replication string, dataCenter string, maxFilenameLength uint32, notifyFn func()) *Filer {
	f := &Filer{
		MasterClient:        wdclient.NewMasterClient(grpcDialOption, filerGroup, cluster.FilerType, filerHost, dataCenter, "", masters),
		fileIdDeletionQueue: util.NewUnboundedQueue(),
		GrpcDialOption:      grpcDialOption,
		FilerConf:           NewFilerConf(),
		RemoteStorage:       NewFilerRemoteStorage(),
		UniqueFilerId:       util.RandomInt32(),
		Dlm:                 lock_manager.NewDistributedLockManager(filerHost),
		MaxFilenameLength:   maxFilenameLength,
	}
	if f.UniqueFilerId < 0 {
		f.UniqueFilerId = -f.UniqueFilerId
	}

	f.LocalMetaLogBuffer = log_buffer.NewLogBuffer("local", LogFlushInterval, f.logFlushFunc, nil, notifyFn)
	f.metaLogCollection = collection
	f.metaLogReplication = replication

	go f.loopProcessingDeletion()

	return f
}

// MaybeBootstrapFromOnePeer copies the namespace of the earliest peer into
// a fresh store (plan 1.5, 8.22 item 9). The peer may hide inline content
// until this filer holds the internal read token; such entries are never
// stored and the copy is retried every BootstrapRetryInterval, for at most
// BootstrapRetryLimit, or until ctx is cancelled (the master refused this
// filer: context.Cause). since is the bootstrap cut (BootstrapSince): the
// copy gives the namespace as of now, the metadata events from since give
// every change after the cut, so a copy interrupted and resumed later, or
// repeated after a restart, still ends complete once the events are
// replayed. The peer must still hold its persisted log from since on.
func (f *Filer) MaybeBootstrapFromOnePeer(ctx context.Context, self pb.ServerAddress, existingNodes []*master_pb.ClusterNodeUpdate, since time.Time) (err error) {
	if len(existingNodes) == 0 {
		return
	}
	sort.Slice(existingNodes, func(i, j int) bool {
		return existingNodes[i].CreatedAtNs < existingNodes[j].CreatedAtNs
	})
	earliestNode := existingNodes[0]
	if earliestNode.Address == string(self) {
		return
	}

	glog.V(0).Infof("bootstrap from %v clientId:%d, cut at %v", earliestNode.Address, f.UniqueFilerId, since)

	deadline := time.Now().Add(BootstrapRetryLimit)
	for {
		err = pb.WithFilerClient(false, f.UniqueFilerId, pb.ServerAddress(earliestNode.Address), f.GrpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
			if err := checkBootstrapLogCoverage(ctx, client, since); err != nil {
				return err
			}
			return filer_pb.StreamBfsWithContext(WithInternalReadToken(ctx), client, "/", since.UnixNano(), func(parentPath util.FullPath, entry *filer_pb.Entry) error {
				if entry.ContentHidden {
					return fmt.Errorf("%s: %w", parentPath.Child(entry.Name), filer_pb.ErrContentHidden)
				}
				return f.Store.InsertEntry(ctx, FromPbEntry(string(parentPath), entry))
			})
		})
		if ctx.Err() != nil {
			return fmt.Errorf("bootstrap from %v stopped: %w", earliestNode.Address, context.Cause(ctx))
		}
		if !errors.Is(err, filer_pb.ErrContentHidden) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bootstrap from %v: %v; the peer still hides content after %v (no internal read token from the master; is this filer admitted?)", earliestNode.Address, err, BootstrapRetryLimit)
		}
		glog.Warningf("bootstrap from %v: %v; retrying", earliestNode.Address, err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("bootstrap from %v stopped: %w", earliestNode.Address, context.Cause(ctx))
		case <-time.After(BootstrapRetryInterval):
		}
	}
}

var (
	// BootstrapRetryInterval is how long a bootstrap waits before retrying
	// when the peer hid inline content; BootstrapRetryLimit bounds the
	// retries.
	BootstrapRetryInterval = 5 * time.Second
	BootstrapRetryLimit    = 5 * time.Minute
)

// FilerBootstrapSince is the store key holding the bootstrap cut of a store
// whose bootstrap has not completed (unix nanoseconds).
const FilerBootstrapSince = "filer.bootstrap.since"

// BootstrapSince returns the bootstrap cut of this store: the one an
// earlier, unfinished bootstrap persisted, or startFrom, persisted now.
// The metadata subscriptions start from it (AggregateFromPeers) so that
// nothing between the first attempt and the completed copy is missed.
func (f *Filer) BootstrapSince(startFrom time.Time) (time.Time, error) {
	ctx := context.Background()
	value, err := f.Store.KvGet(ctx, []byte(FilerBootstrapSince))
	switch {
	case err == nil && len(value) == 8:
		return time.Unix(0, int64(util.BytesToUint64(value))), nil
	case err == nil && len(value) != 0:
		return time.Time{}, fmt.Errorf("read %s: unexpected value %x", FilerBootstrapSince, value)
	case err != nil && err != ErrKvNotFound:
		return time.Time{}, fmt.Errorf("read %s: %v", FilerBootstrapSince, err)
	}
	value = make([]byte, 8)
	util.Uint64toBytes(value, uint64(startFrom.UnixNano()))
	if err := f.Store.KvPut(ctx, []byte(FilerBootstrapSince), value); err != nil {
		return time.Time{}, fmt.Errorf("write %s: %v", FilerBootstrapSince, err)
	}
	return startFrom, nil
}

// FinishBootstrap marks the store initialized: the bootstrap cut is dropped
// and the store signature written (SetStore generated it). Until then a
// restart finds the store fresh and bootstraps again from the same cut.
func (f *Filer) FinishBootstrap() error {
	ctx := context.Background()
	if err := f.Store.KvDelete(ctx, []byte(FilerBootstrapSince)); err != nil && err != ErrKvNotFound {
		return fmt.Errorf("delete %s: %v", FilerBootstrapSince, err)
	}
	return f.PersistStoreSignature()
}

// checkBootstrapLogCoverage refuses a bootstrap whose cut the peer's
// persisted metadata log no longer reaches back to: the events from the cut
// would silently start later and deletions made meanwhile would stay in
// this store. The log is kept in UTC day directories; a peer without any
// has nothing to replay.
func checkBootstrapLogCoverage(ctx context.Context, client filer_pb.SeaweedFilerClient, since time.Time) error {
	var earliest string
	err := filer_pb.SeaweedList(client, SystemLogDir, "", func(entry *filer_pb.Entry, isLast bool) error {
		if entry.IsDirectory && (earliest == "" || entry.Name < earliest) {
			earliest = entry.Name
		}
		return nil
	}, "", false, 1)
	if err != nil && !strings.Contains(err.Error(), filer_pb.ErrNotFound.Error()) {
		return fmt.Errorf("list the peer's metadata log %s: %v", SystemLogDir, err)
	}
	cutDay := since.UTC().Format("2006-01-02")
	if earliest != "" && earliest > cutDay {
		return fmt.Errorf("the peer's metadata log starts on %s, after the bootstrap cut %v of an earlier attempt; changes since then cannot be replayed: empty this filer's store and start again", earliest, since.UTC().Format(time.RFC3339))
	}
	return nil
}

func (f *Filer) AggregateFromPeers(self pb.ServerAddress, existingNodes []*master_pb.ClusterNodeUpdate, startFrom time.Time) {

	var snapshot []pb.ServerAddress
	for _, node := range existingNodes {
		address := pb.ServerAddress(node.Address)
		snapshot = append(snapshot, address)
	}
	f.Dlm.LockRing.SetSnapshot(snapshot)
	glog.V(0).Infof("%s aggregate from peers %+v", self, snapshot)

	f.MetaAggregator = NewMetaAggregator(f, self, f.GrpcDialOption)
	f.MasterClient.SetOnPeerUpdateFn(func(update *master_pb.ClusterNodeUpdate, startFrom time.Time) {
		if update.NodeType != cluster.FilerType {
			return
		}
		address := pb.ServerAddress(update.Address)

		if update.IsAdd {
			f.Dlm.LockRing.AddServer(address)
		} else {
			f.Dlm.LockRing.RemoveServer(address)
		}
		f.MetaAggregator.OnPeerUpdate(update, startFrom)
	})

	for _, peerUpdate := range existingNodes {
		f.MetaAggregator.OnPeerUpdate(peerUpdate, startFrom)
	}

}

func (f *Filer) ListExistingPeerUpdates(ctx context.Context) (existingNodes []*master_pb.ClusterNodeUpdate) {
	return cluster.ListExistingPeerUpdates(f.GetMaster(ctx), f.GrpcDialOption, f.MasterClient.FilerGroup, cluster.FilerType)
}

// SetStore installs the store. A store without a signature is fresh: a
// signature is generated but written only when the store is initialized
// (FinishBootstrap), so a bootstrap cut short by a restart leaves the store
// fresh instead of half a namespace that passes for a complete one.
func (f *Filer) SetStore(store FilerStore) (isFresh bool) {
	f.Store = NewFilerStoreWrapper(store)

	return f.setOrLoadFilerStoreSignature(store)
}

func (f *Filer) setOrLoadFilerStoreSignature(store FilerStore) (isFresh bool) {
	storeIdBytes, err := store.KvGet(context.Background(), []byte(FilerStoreId))
	if err == ErrKvNotFound || err == nil && len(storeIdBytes) == 0 {
		f.Signature = util.RandomInt32()
		glog.V(0).Infof("fresh store, %s will be %d once initialized", FilerStoreId, f.Signature)
		return true
	} else if err == nil && len(storeIdBytes) == 4 {
		f.Signature = int32(util.BytesToUint32(storeIdBytes))
		glog.V(0).Infof("existing %s = %d", FilerStoreId, f.Signature)
	} else {
		glog.Fatalf("read %v=%v : %v", FilerStoreId, string(storeIdBytes), err)
	}
	return false
}

// PersistStoreSignature writes the signature SetStore generated for a fresh
// store; peers read it (filer.store.id) to follow this filer.
func (f *Filer) PersistStoreSignature() error {
	storeIdBytes := make([]byte, 4)
	util.Uint32toBytes(storeIdBytes, uint32(f.Signature))
	if err := f.Store.KvPut(context.Background(), []byte(FilerStoreId), storeIdBytes); err != nil {
		return fmt.Errorf("set %s=%d : %v", FilerStoreId, f.Signature, err)
	}
	glog.V(0).Infof("create %s to %d", FilerStoreId, f.Signature)
	return nil
}

func (f *Filer) GetStore() (store FilerStore) {
	return f.Store
}

func (fs *Filer) GetMaster(ctx context.Context) pb.ServerAddress {
	return fs.MasterClient.GetMaster(ctx)
}

func (fs *Filer) KeepMasterClientConnected(ctx context.Context) {
	fs.MasterClient.KeepConnectedToMaster(ctx)
}

func (f *Filer) BeginTransaction(ctx context.Context) (context.Context, error) {
	return f.Store.BeginTransaction(ctx)
}

func (f *Filer) CommitTransaction(ctx context.Context) error {
	return f.Store.CommitTransaction(ctx)
}

func (f *Filer) RollbackTransaction(ctx context.Context) error {
	return f.Store.RollbackTransaction(ctx)
}

func (f *Filer) CreateEntry(ctx context.Context, entry *Entry, o_excl bool, isFromOtherCluster bool, signatures []int32, skipCreateParentDir bool, maxFilenameLength uint32) error {

	if string(entry.FullPath) == "/" {
		return nil
	}

	if entry.FullPath.IsLongerFileName(maxFilenameLength) {
		return fmt.Errorf("entry name too long")
	}

	oldEntry, _ := f.FindEntry(ctx, entry.FullPath)

	/*
		if !hasWritePermission(lastDirectoryEntry, entry) {
			glog.V(0).Infof("directory %s: %v, entry: uid=%d gid=%d",
				lastDirectoryEntry.FullPath, lastDirectoryEntry.Attr, entry.Uid, entry.Gid)
			return fmt.Errorf("no write permission in folder %v", lastDirectoryEntry.FullPath)
		}
	*/

	if oldEntry == nil {

		if !skipCreateParentDir {
			dirParts := strings.Split(string(entry.FullPath), "/")
			if err := f.ensureParentDirectoryEntry(ctx, entry, dirParts, len(dirParts)-1, isFromOtherCluster); err != nil {
				return err
			}
		}

		glog.V(4).Infof("InsertEntry %s: new entry: %v", entry.FullPath, entry.Name())
		if err := f.Store.InsertEntry(ctx, entry); err != nil {
			glog.Errorf("insert entry %s: %v", entry.FullPath, err)
			return fmt.Errorf("insert entry %s: %v", entry.FullPath, err)
		}
	} else {
		if o_excl {
			glog.V(3).Infof("EEXIST: entry %s already exists", entry.FullPath)
			return fmt.Errorf("EEXIST: entry %s already exists", entry.FullPath)
		}
		glog.V(4).Infof("UpdateEntry %s: old entry: %v", entry.FullPath, oldEntry.Name())
		if err := f.UpdateEntry(ctx, oldEntry, entry); err != nil {
			glog.Errorf("update entry %s: %v", entry.FullPath, err)
			return fmt.Errorf("update entry %s: %v", entry.FullPath, err)
		}
	}

	f.NotifyUpdateEvent(ctx, oldEntry, entry, true, isFromOtherCluster, signatures)

	f.deleteChunksIfNotNew(oldEntry, entry)

	glog.V(4).Infof("CreateEntry %s: created", entry.FullPath)

	return nil
}

func (f *Filer) ensureParentDirectoryEntry(ctx context.Context, entry *Entry, dirParts []string, level int, isFromOtherCluster bool) (err error) {

	if level == 0 {
		return nil
	}

	dirPath := "/" + util.Join(dirParts[:level]...)
	// fmt.Printf("%d dirPath: %+v\n", level, dirPath)

	// check the store directly
	glog.V(4).Infof("find uncached directory: %s", dirPath)
	dirEntry, _ := f.FindEntry(ctx, util.FullPath(dirPath))

	// no such existing directory
	if dirEntry == nil {

		// fmt.Printf("dirParts: %v %v %v\n", dirParts[0], dirParts[1], dirParts[2])
		// dirParts[0] == "" and dirParts[1] == "buckets"
		if len(dirParts) >= 3 && dirParts[1] == "buckets" {
			if err := s3bucket.VerifyS3BucketName(dirParts[2]); err != nil {
				return fmt.Errorf("invalid bucket name %s: %v", dirParts[2], err)
			}
		}

		// ensure parent directory
		if err = f.ensureParentDirectoryEntry(ctx, entry, dirParts, level-1, isFromOtherCluster); err != nil {
			return err
		}

		// create the directory
		now := time.Now()

		dirEntry = &Entry{
			FullPath: util.FullPath(dirPath),
			Attr: Attr{
				Mtime:      now,
				Crtime:     now,
				Mode:       os.ModeDir | entry.Mode | 0111,
				Uid:        entry.Uid,
				Gid:        entry.Gid,
				UserName:   entry.UserName,
				GroupNames: entry.GroupNames,
			},
		}

		glog.V(2).Infof("create directory: %s %v", dirPath, dirEntry.Mode)
		mkdirErr := f.Store.InsertEntry(ctx, dirEntry)
		if mkdirErr != nil {
			if _, err := f.FindEntry(ctx, util.FullPath(dirPath)); err == filer_pb.ErrNotFound {
				glog.V(3).Infof("mkdir %s: %v", dirPath, mkdirErr)
				return fmt.Errorf("mkdir %s: %v", dirPath, mkdirErr)
			}
		} else {
			if !strings.HasPrefix("/"+util.Join(dirParts[:]...), SystemLogDir) {
				f.NotifyUpdateEvent(ctx, nil, dirEntry, false, isFromOtherCluster, nil)
			}
		}

	} else if !dirEntry.IsDirectory() {
		glog.Errorf("CreateEntry %s: %s should be a directory", entry.FullPath, dirPath)
		return fmt.Errorf("%s is a file", dirPath)
	}

	return nil
}

func (f *Filer) UpdateEntry(ctx context.Context, oldEntry, entry *Entry) (err error) {
	if oldEntry != nil {
		entry.Attr.Crtime = oldEntry.Attr.Crtime
		if oldEntry.IsDirectory() && !entry.IsDirectory() {
			glog.Errorf("existing %s is a directory", oldEntry.FullPath)
			return fmt.Errorf("existing %s is a directory", oldEntry.FullPath)
		}
		if !oldEntry.IsDirectory() && entry.IsDirectory() {
			glog.Errorf("existing %s is a file", oldEntry.FullPath)
			return fmt.Errorf("existing %s is a file", oldEntry.FullPath)
		}
	}
	return f.Store.UpdateEntry(ctx, entry)
}

var (
	Root = &Entry{
		FullPath: "/",
		Attr: Attr{
			Mtime:  time.Now(),
			Crtime: time.Now(),
			Mode:   os.ModeDir | 0755,
			Uid:    OS_UID,
			Gid:    OS_GID,
		},
	}
)

func (f *Filer) FindEntry(ctx context.Context, p util.FullPath) (entry *Entry, err error) {

	if string(p) == "/" {
		return Root, nil
	}
	entry, err = f.Store.FindEntry(ctx, p)
	if entry != nil && entry.TtlSec > 0 {
		if entry.Crtime.Add(time.Duration(entry.TtlSec) * time.Second).Before(time.Now()) {
			f.Store.DeleteOneEntry(ctx, entry)
			return nil, filer_pb.ErrNotFound
		}
	}
	return

}

func (f *Filer) doListDirectoryEntries(ctx context.Context, p util.FullPath, startFileName string, inclusive bool, limit int64, prefix string, eachEntryFunc ListEachEntryFunc) (expiredCount int64, lastFileName string, err error) {
	lastFileName, err = f.Store.ListDirectoryPrefixedEntries(ctx, p, startFileName, inclusive, limit, prefix, func(entry *Entry) bool {
		select {
		case <-ctx.Done():
			return false
		default:
			if entry.TtlSec > 0 {
				if entry.Crtime.Add(time.Duration(entry.TtlSec) * time.Second).Before(time.Now()) {
					f.Store.DeleteOneEntry(ctx, entry)
					expiredCount++
					return true
				}
			}
			return eachEntryFunc(entry)
		}
	})
	if err != nil {
		return expiredCount, lastFileName, err
	}
	return
}

func (f *Filer) Shutdown() {
	f.LocalMetaLogBuffer.ShutdownLogBuffer()
	f.Store.Shutdown()
}
