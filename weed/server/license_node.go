package weed_server

import (
	"context"
	"net/http"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
)

// Paths of license_denied_reads_total.
const (
	licensePathVolumeHttp = "volume_http"
	licensePathVolumeGrpc = "volume_grpc"
	licensePathFilerHttp  = "filer_http"
	licensePathFilerProxy = "filer_proxy"
	licensePathFilerGrpc  = "filer_grpc_hidden"
)

// newLicenseNode creates the license state of a volume server or filer,
// cached in cacheDir; an empty cacheDir disables the cache.
func newLicenseNode(cacheDir string) *license.Node {
	trust, err := license.DefaultTrustStore()
	if err != nil {
		glog.Fatalf("license: %v", err)
	}
	cachePath := ""
	if cacheDir != "" {
		cachePath = filepath.Join(cacheDir, license.CacheFileName)
	}
	n := license.NewNode(trust, cachePath)
	go n.Run(nil)
	return n
}

func licenseUpdateFromPb(u *master_pb.LicenseUpdate) *license.Update {
	return &license.Update{
		NewestCertificate: u.NewestCertificate,
		PrevCertificate:   u.PrevCertificate,
		MasterNowUnixMs:   u.MasterNowUnixMs,
		ClusterID:         u.ClusterId,
		MasterDenyReason:  u.MasterDenyReason,
		InternalReadKey:   u.InternalReadKey,
		StateVersion:      u.StateVersion,
	}
}

// licenseHttpReadAllowed guards a GET of file content (plan 1.2 V1, F1,
// F2). It writes the 403 and returns false when the read is denied. With
// acceptToken, a valid internal read token lets filer maintenance reads
// through while the license denies reads.
func licenseHttpReadAllowed(n *license.Node, w http.ResponseWriter, r *http.Request, server, path string, acceptToken bool) bool {
	guard := n.Guard()
	ok, snap := guard.AllowRead()
	if ok {
		return true
	}
	if acceptToken && guard.InternalTokenValid(r.Header.Get(license.InternalReadHeader)) {
		return true
	}
	guard.WriteDenied(w, snap)
	licenseNoteDenied(n, snap, server, path)
	return false
}

// licenseGrpcReadAllowed guards a volume gRPC RPC that returns data
// (plan 1.2 G2). Streams are checked once, when they start.
func licenseGrpcReadAllowed(n *license.Node, ctx context.Context) error {
	guard := n.Guard()
	ok, snap := guard.AllowRead()
	if ok || licenseGrpcTokenValid(n, ctx) {
		return nil
	}
	licenseNoteDenied(n, snap, "volumeServer", licensePathVolumeGrpc)
	return status.Error(codes.PermissionDenied, "license: "+string(snap.Reason(guard.Now())))
}

// licenseGrpcTokenValid checks the internal read token in the incoming
// gRPC metadata.
func licenseGrpcTokenValid(n *license.Node, ctx context.Context) bool {
	values := metadata.ValueFromIncomingContext(ctx, license.InternalReadMetadataKey)
	return len(values) == 1 && n.Guard().InternalTokenValid(values[0])
}

// licenseOutgoingContext adds the internal read token to a gRPC call made
// for internal maintenance, if this node has the key.
func licenseOutgoingContext(n *license.Node, ctx context.Context) context.Context {
	key := n.InternalKey()
	if key == nil {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, license.InternalReadMetadataKey, license.EncodeInternalToken(key))
}

// licenseInternalToken returns the encoded internal read token, or "".
func (vs *VolumeServer) licenseInternalToken() string {
	if key := vs.license.InternalKey(); key != nil {
		return license.EncodeInternalToken(key)
	}
	return ""
}

func licenseNoteDenied(n *license.Node, snap *license.Snapshot, server, path string) {
	stats.LicenseDeniedReadsCounter.WithLabelValues(server, path).Inc()
	n.LogDenied(path, snap.Reason(n.Guard().Now()))
}
