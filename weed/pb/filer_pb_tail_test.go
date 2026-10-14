package pb

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

type fakeSubscribeStream struct {
	grpc.ClientStream
	events []*filer_pb.SubscribeMetadataResponse
}

func (s *fakeSubscribeStream) Recv() (*filer_pb.SubscribeMetadataResponse, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	e := s.events[0]
	s.events = s.events[1:]
	return e, nil
}

type fakeSubscribeClient struct {
	filer_pb.SeaweedFilerClient
	stream *fakeSubscribeStream
}

func (c *fakeSubscribeClient) SubscribeMetadata(ctx context.Context, in *filer_pb.SubscribeMetadataRequest, opts ...grpc.CallOption) (filer_pb.SeaweedFiler_SubscribeMetadataClient, error) {
	return c.stream, nil
}

func TestFollowMetadataFailOnHiddenContent(t *testing.T) {
	events := func() []*filer_pb.SubscribeMetadataResponse {
		return []*filer_pb.SubscribeMetadataResponse{
			{TsNs: 1, EventNotification: &filer_pb.EventNotification{NewEntry: &filer_pb.Entry{Name: "a", Content: []byte("x")}}},
			{TsNs: 2, EventNotification: &filer_pb.EventNotification{NewEntry: &filer_pb.Entry{Name: "b", ContentHidden: true}}},
			{TsNs: 3, EventNotification: &filer_pb.EventNotification{NewEntry: &filer_pb.Entry{Name: "c"}}},
		}
	}
	for _, fail := range []bool{true, false} {
		var processed []int64
		option := &MetadataFollowOption{FailOnHiddenContent: fail}
		err := makeSubscribeMetadataFunc(option, func(resp *filer_pb.SubscribeMetadataResponse) error {
			processed = append(processed, resp.TsNs)
			return nil
		})(&fakeSubscribeClient{stream: &fakeSubscribeStream{events: events()}})
		if fail {
			if !errors.Is(err, filer_pb.ErrContentHidden) || len(processed) != 1 || option.StartTsNs != 1 {
				t.Fatalf("fail: err %v, processed %v, resume at %d", err, processed, option.StartTsNs)
			}
		} else if err != nil || len(processed) != 3 {
			t.Fatalf("follow: err %v, processed %v", err, processed)
		}
	}
}
