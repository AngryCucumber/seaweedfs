package filer

import (
	"context"
	"net/http"
	"sync/atomic"

	"google.golang.org/grpc/metadata"

	"github.com/seaweedfs/seaweedfs/weed/license"
)

// internalRead is the license internal read token of this filer process.
type internalRead struct {
	token  string
	header http.Header
}

var internalReadToken atomic.Pointer[internalRead]

// SetInternalReadToken sets the internal read key received from the
// master. The filer's own maintenance reads (manifest resolution, metadata
// log replay, chunk merge, configuration files) present it to volume
// servers, and filers present it to each other, so they keep working while
// the license denies reads (plan 1.4). Any other key size clears it.
func SetInternalReadToken(key []byte) {
	if len(key) != license.InternalKeySize {
		internalReadToken.Store(nil)
		return
	}
	token := license.EncodeInternalToken(key)
	if cur := internalReadToken.Load(); cur != nil && cur.token == token {
		return
	}
	header := http.Header{}
	header.Set(license.InternalReadHeader, token)
	internalReadToken.Store(&internalRead{token: token, header: header})
}

// internalReadHeader returns the header carrying the internal read token,
// or nil. Only fetchWholeChunk, ChunkStreamReader and StreamContent use it;
// user reads never carry it.
func internalReadHeader() http.Header {
	if t := internalReadToken.Load(); t != nil {
		return t.header
	}
	return nil
}

// WithInternalReadToken adds the internal read token to the context of a
// gRPC call to another filer, for metadata subscriptions and bootstrap.
func WithInternalReadToken(ctx context.Context) context.Context {
	if t := internalReadToken.Load(); t != nil {
		return metadata.AppendToOutgoingContext(ctx, license.InternalReadMetadataKey, t.token)
	}
	return ctx
}
