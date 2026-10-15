package filer

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

// A chunk truncated in place (weed mount SetAttr) keeps the file id of its
// needle and only gets a smaller Size. Reading the whole chunk returns the
// whole needle, and only the first Size bytes of it belong to the file.
func TestStreamChunkTruncatedInPlace(t *testing.T) {
	util_http.InitGlobalHttpClient()

	needles := map[string][]byte{
		"3,01637037d6": []byte("world"),
		"3,02637037d6": []byte("ab"),
	}
	for _, mode := range []string{"plain", "gzip", "cipher"} {
		t.Run(mode, func(t *testing.T) {
			cipherKeys := map[string][]byte{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fileId := strings.TrimPrefix(r.URL.Path, "/")
				data, found := needles[fileId]
				if !found {
					http.NotFound(w, r)
					return
				}
				switch {
				case mode == "cipher":
					encrypted, err := util.Encrypt(data, cipherKeys[fileId])
					if err != nil {
						t.Error(err)
					}
					w.Write(encrypted)
				case mode == "gzip" && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"):
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzip.NewWriter(w)
					gz.Write(data)
					gz.Close()
				default:
					// Like the volume server: a range of the plain data.
					http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
				}
			}))
			t.Cleanup(srv.Close)
			lookup := func(fileId string) ([]string, error) { return []string{srv.URL + "/" + fileId}, nil }

			chunks := []*filer_pb.FileChunk{
				{FileId: "3,01637037d6", Offset: 0, Size: 3},
				{FileId: "3,02637037d6", Offset: 3, Size: 2},
			}
			for _, chunk := range chunks {
				chunk.IsCompressed = mode == "gzip"
				if mode == "cipher" {
					chunk.CipherKey = util.GenCipherKey()
					cipherKeys[chunk.FileId] = chunk.CipherKey
				}
			}
			// The file was grown to 7 bytes after the truncate.
			want := "worab\x00\x00"

			streamFn, err := PrepareStreamContentWithThrottler(lookupFn(lookup), noJwtFunc, chunks, 0, int64(len(want)), 0)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := streamFn(&buf); err != nil || buf.String() != want {
				t.Fatalf("user stream: %v %q", err, buf.String())
			}

			buf.Reset()
			if err := StreamContent(lookupFn(lookup), &buf, chunks, 0, int64(len(want))); err != nil || buf.String() != want {
				t.Fatalf("StreamContent: %v %q", err, buf.String())
			}

			// A range inside the truncated chunk.
			streamFn, err = PrepareStreamContentWithThrottler(lookupFn(lookup), noJwtFunc, chunks, 1, 4, 0)
			if err != nil {
				t.Fatal(err)
			}
			buf.Reset()
			if err := streamFn(&buf); err != nil || buf.String() != want[1:5] {
				t.Fatalf("range stream: %v %q", err, buf.String())
			}

			read, err := io.ReadAll(doNewChunkStreamReader(lookup, chunks))
			if err != nil || string(read) != want[:5] {
				t.Fatalf("ChunkStreamReader: %v %q", err, read)
			}

			// A whole chunk fetch with no size still reads all of the needle.
			buf.Reset()
			if err := fetchWholeChunk(&buf, lookup, chunks[0].FileId, chunks[0].CipherKey, chunks[0].IsCompressed); err != nil || buf.String() != "world" {
				t.Fatalf("fetchWholeChunk: %v %q", err, buf.String())
			}
		})
	}
}
