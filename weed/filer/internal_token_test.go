package filer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

// tokenRecorder serves body and records the internal read token of each
// request.
type tokenRecorder struct {
	sync.Mutex
	tokens []string
}

func (rec *tokenRecorder) server(t *testing.T, body []byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Lock()
		rec.tokens = append(rec.tokens, r.Header.Get(license.InternalReadHeader))
		rec.Unlock()
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rec *tokenRecorder) all(t *testing.T, want string) {
	t.Helper()
	rec.Lock()
	defer rec.Unlock()
	if len(rec.tokens) == 0 {
		t.Fatal("no request")
	}
	for _, got := range rec.tokens {
		if got != want {
			t.Fatalf("token %q, want %q", got, want)
		}
	}
	rec.tokens = nil
}

type lookupFn wdclient.LookupFileIdFunctionType

func (f lookupFn) GetLookupFileIdFunction() wdclient.LookupFileIdFunctionType {
	return wdclient.LookupFileIdFunctionType(f)
}

func TestInternalReadToken(t *testing.T) {
	util_http.InitGlobalHttpClient()
	key := bytes.Repeat([]byte{7}, license.InternalKeySize)
	token := license.EncodeInternalToken(key)
	SetInternalReadToken(key)
	t.Cleanup(func() { SetInternalReadToken(nil) })

	if got := internalReadHeader().Get(license.InternalReadHeader); got != token {
		t.Fatalf("header %q", got)
	}
	md, _ := metadata.FromOutgoingContext(WithInternalReadToken(context.Background()))
	if got := md.Get(license.InternalReadMetadataKey); len(got) != 1 || got[0] != token {
		t.Fatalf("metadata %v", got)
	}

	data := []byte("chunk data")
	for _, cipher := range []bool{false, true} {
		var rec tokenRecorder
		body, cipherKey := data, []byte(nil)
		if cipher {
			cipherKey = util.GenCipherKey()
			body, _ = util.Encrypt(data, cipherKey)
		}
		srv := rec.server(t, body)
		lookup := func(fileId string) ([]string, error) { return []string{srv.URL + "/" + fileId}, nil }
		chunks := []*filer_pb.FileChunk{{FileId: "3,01637037d6", Size: uint64(len(data)), CipherKey: cipherKey}}

		// Maintenance reads carry the token.
		var buf bytes.Buffer
		if err := fetchWholeChunk(&buf, lookup, "3,01637037d6", cipherKey, false); err != nil || buf.String() != string(data) {
			t.Fatalf("fetchWholeChunk cipher=%v: %v %q", cipher, err, buf.String())
		}
		rec.all(t, token)

		buf.Reset()
		if err := StreamContent(lookupFn(lookup), &buf, chunks, 0, int64(len(data))); err != nil || buf.String() != string(data) {
			t.Fatalf("StreamContent cipher=%v: %v %q", cipher, err, buf.String())
		}
		rec.all(t, token)

		read, err := io.ReadAll(doNewChunkStreamReader(lookup, chunks))
		if err != nil || string(read) != string(data) {
			t.Fatalf("ChunkStreamReader cipher=%v: %v %q", cipher, err, read)
		}
		rec.all(t, token)

		// User reads never do.
		streamFn, err := PrepareStreamContentWithThrottler(lookupFn(lookup), noJwtFunc, chunks, 0, int64(len(data)), 0)
		if err != nil {
			t.Fatal(err)
		}
		buf.Reset()
		if err := streamFn(&buf); err != nil || buf.String() != string(data) {
			t.Fatalf("user stream cipher=%v: %v %q", cipher, err, buf.String())
		}
		rec.all(t, "")
	}

	SetInternalReadToken([]byte("short"))
	if internalReadHeader() != nil {
		t.Fatal("a short key was accepted")
	}
	if WithInternalReadToken(context.Background()) != context.Background() {
		t.Fatal("token added without a key")
	}
}

func TestReadEntryRefusesHiddenContent(t *testing.T) {
	hidden := &filer_pb.Entry{Name: "a", ContentHidden: true, Attributes: &filer_pb.FuseAttributes{FileSize: 3}}
	if !filer_pb.EventHasHiddenContent(&filer_pb.EventNotification{NewEntry: hidden}) {
		t.Fatal("new entry")
	}
	if filer_pb.EventHasHiddenContent(&filer_pb.EventNotification{}) || filer_pb.EventHasHiddenContent(nil) {
		t.Fatal("empty event")
	}
	if err := Replay(nil, &filer_pb.SubscribeMetadataResponse{EventNotification: &filer_pb.EventNotification{OldEntry: hidden}}); err != filer_pb.ErrContentHidden {
		t.Fatalf("Replay: %v", err)
	}
}
