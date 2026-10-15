package mount

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func (fh *FileHandle) lockForRead(startOffset int64, size int) {
	fh.dirtyPages.LockForRead(startOffset, startOffset+int64(size))
}
func (fh *FileHandle) unlockForRead(startOffset int64, size int) {
	fh.dirtyPages.UnlockForRead(startOffset, startOffset+int64(size))
}

func (fh *FileHandle) readFromDirtyPages(buff []byte, startOffset int64, tsNs int64) (maxStop int64) {
	maxStop = fh.dirtyPages.ReadDirtyDataAt(buff, startOffset, tsNs)
	return
}

func (fh *FileHandle) readFromChunks(buff []byte, offset int64) (int64, int64, error) {
	fileFullPath := fh.FullPath()

	fh.entryLock.RLock()
	// The download installs chunks and their chunk group, so it runs
	// outside the read lock. A handle failing reads keeps failing.
	if fh.GetEntry().IsInRemoteOnly() && fh.readError() == nil {
		fh.entryLock.RUnlock()
		glog.V(4).Infof("download remote entry %s", fileFullPath)
		err := fh.downloadRemoteEntry()
		if err != nil {
			glog.V(1).Infof("download remote entry %s: %v", fileFullPath, err)
			return 0, 0, err
		}
		fh.entryLock.RLock()
	}
	defer fh.entryLock.RUnlock()

	entry := fh.GetEntry()

	fileSize := int64(entry.Attributes.FileSize)
	if fileSize == 0 {
		fileSize = int64(filer.FileSize(entry.GetEntry()))
	}

	if fileSize == 0 {
		glog.V(1).Infof("empty fh %v", fileFullPath)
		return 0, 0, io.EOF
	} else if offset == fileSize {
		return 0, 0, io.EOF
	} else if offset >= fileSize {
		glog.V(1).Infof("invalid read, fileSize %d, offset %d for %s", fileSize, offset, fileFullPath)
		return 0, 0, io.EOF
	}

	if err := fh.readError(); err != nil {
		return 0, 0, err
	}

	if offset < int64(len(entry.Content)) {
		totalRead := copy(buff, entry.Content[offset:])
		glog.V(4).Infof("file handle read cached %s [%d,%d] %d", fileFullPath, offset, offset+int64(totalRead), totalRead)
		return int64(totalRead), 0, nil
	}

	totalRead, ts, err := fh.entryChunkGroup.ReadDataAt(fileSize, buff, offset)

	if err != nil && err != io.EOF {
		glog.Errorf("file handle read %s: %v", fileFullPath, err)
	}

	// glog.V(4).Infof("file handle read %s [%d,%d] %d : %v", fileFullPath, offset, offset+int64(totalRead), totalRead, err)

	return int64(totalRead), ts, err
}

// cacheRemoteObject asks the filer to cache a remote object in the
// cluster; tests replace it.
var cacheRemoteObject = func(wfs *WFS, request *filer_pb.CacheRemoteObjectToLocalClusterRequest) (resp *filer_pb.CacheRemoteObjectToLocalClusterResponse, err error) {
	err = wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		resp, err = client.CacheRemoteObjectToLocalCluster(context.Background(), request)
		return err
	})
	return
}

// downloadRemoteEntry has the filer cache a remote-only file in the
// cluster and installs the cached entry (takeCachedEntry). One download
// runs at a time per handle; one that finds the file cached already does
// nothing. The meta cache takes the filer's entry as it is.
func (fh *FileHandle) downloadRemoteEntry() error {
	fh.remoteDownloadLock.Lock()
	defer fh.remoteDownloadLock.Unlock()
	fh.entryLock.RLock()
	remoteOnly := fh.GetEntry().IsInRemoteOnly()
	fh.entryLock.RUnlock()
	if !remoteOnly {
		return nil
	}

	fileFullPath := fh.FullPath()
	dir, name := fileFullPath.DirAndName()

	request := &filer_pb.CacheRemoteObjectToLocalClusterRequest{
		Directory: dir,
		Name:      name,
	}
	glog.V(4).Infof("download entry: %v", request)
	resp, err := cacheRemoteObject(fh.wfs, request)
	if err != nil {
		return fmt.Errorf("CacheRemoteObjectToLocalCluster file %s: %v", fileFullPath, err)
	}
	if resp.GetEntry() == nil {
		return fmt.Errorf("CacheRemoteObjectToLocalCluster file %s: no longer a remote object", fileFullPath)
	}
	cached := resp.Entry
	group, err := filer.NewChunkGroup(fh.wfs.LookupFn(), fh.wfs.chunkCache, cached.Chunks)
	if err != nil {
		return err
	}
	fh.wfs.metaCache.InsertEntry(context.Background(), fh.wfs.localEntryFromFiler(dir, cached))
	fh.takeCachedEntry(cached, group)
	return nil
}

// cacheRemoteBeforeChange caches the content of a remote-only file before
// a write or size change on this handle, so the change applies to it like
// to any chunked file: written data is newer than the cached chunks, and a
// truncation cuts them.
func (fh *FileHandle) cacheRemoteBeforeChange() error {
	fh.entryLock.RLock()
	remoteOnly := fh.GetEntry().IsInRemoteOnly()
	fh.entryLock.RUnlock()
	if !remoteOnly {
		return nil
	}
	return fh.downloadRemoteEntry()
}

// cacheRemoteEntry caches a remote-only file whose size changes without an
// open handle, and puts the cached content into entry.
func (wfs *WFS) cacheRemoteEntry(path util.FullPath, entry *filer_pb.Entry) error {
	if !entry.IsInRemoteOnly() {
		return nil
	}
	dir, name := path.DirAndName()
	resp, err := cacheRemoteObject(wfs, &filer_pb.CacheRemoteObjectToLocalClusterRequest{Directory: dir, Name: name})
	if err != nil {
		return fmt.Errorf("CacheRemoteObjectToLocalCluster file %s: %v", path, err)
	}
	if resp.GetEntry() == nil {
		return fmt.Errorf("CacheRemoteObjectToLocalCluster file %s: no longer a remote object", path)
	}
	cached := resp.Entry
	wfs.metaCache.InsertEntry(context.Background(), wfs.localEntryFromFiler(dir, cached))
	takeCachedContent(entry, cached)
	return nil
}

// takeCachedEntry installs the entry of a remote-only file just cached in
// the cluster, mapped to local ids, with the chunk group of its chunks.
// Without changes on this handle it replaces the entry; with changes not
// flushed yet, which are attribute changes only because writes and size
// changes cache the file first (cacheRemoteBeforeChange), only the content
// is taken. Another download that already installed its result wins. The
// filer announces each download as an update of the entry, which must not
// replace the handle's entry: it would drop changes made since (ownCacheEvent).
func (fh *FileHandle) takeCachedEntry(cached *filer_pb.Entry, group *filer.ChunkGroup) {
	fh.entryLock.Lock()
	defer fh.entryLock.Unlock()
	if !fh.GetEntry().IsInRemoteOnly() {
		return
	}
	fh.cachedRemote.Store(proto.Clone(&filer_pb.Entry{Chunks: cached.Chunks, RemoteEntry: cached.RemoteEntry}).(*filer_pb.Entry))
	if fh.dirtyMetadata {
		fh.entry.UpdateEntry(func(e *filer_pb.Entry) {
			takeCachedContent(e, cached)
		})
	} else {
		cached.Attributes.FileSize = filer.FileSize(cached)
		fh.entry.SetEntry(cached)
	}
	fh.entryChunkGroup = group
	fh.noteChunks(cached.Chunks, group)
}

// takeCachedContent puts the cached content of a remote object into e: inline
// bytes, chunks, remote object, size and md5.
func takeCachedContent(e, cached *filer_pb.Entry) {
	e.Content, e.ContentHidden, e.Chunks, e.RemoteEntry = cached.Content, cached.ContentHidden, cached.Chunks, cached.RemoteEntry
	e.Attributes.FileSize = filer.FileSize(cached)
	e.Attributes.Md5 = cached.GetAttributes().GetMd5()
}

// ownCacheEvent reports whether entry, the new entry of a metadata event
// for this handle's file, is the result of the handle's own last download
// (takeCachedEntry), and consumes that result. The event then brings
// nothing new. Reads and writes hold the handle lock while downloading, so
// the event is handled after the download installed its result.
func (fh *FileHandle) ownCacheEvent(entry *filer_pb.Entry) bool {
	cached := fh.cachedRemote.Load()
	if cached == nil || entry == nil || !proto.Equal(cached.RemoteEntry, entry.RemoteEntry) || len(cached.Chunks) != len(entry.Chunks) {
		return false
	}
	for i, chunk := range cached.Chunks {
		if chunk.GetFileIdString() != entry.Chunks[i].GetFileIdString() || chunk.Offset != entry.Chunks[i].Offset || chunk.Size != entry.Chunks[i].Size {
			return false
		}
	}
	return fh.cachedRemote.CompareAndSwap(cached, nil)
}
