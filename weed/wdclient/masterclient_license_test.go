package wdclient

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMaybeLicenseReject(t *testing.T) {
	var got []string
	mc := &MasterClient{OnLicenseReject: func(message string) { got = append(got, message) }}
	mc.maybeLicenseReject(status.Error(codes.PermissionDenied, "license: NODE_LIMIT_EXCEEDED filer 2/2"))
	mc.maybeLicenseReject(status.Error(codes.Unavailable, "license: RECONCILING"))
	mc.maybeLicenseReject(status.Error(codes.PermissionDenied, "not a license error"))
	mc.maybeLicenseReject(errors.New("license: plain error"))
	if len(got) != 1 || got[0] != "license: NODE_LIMIT_EXCEEDED filer 2/2" {
		t.Fatalf("got %q", got)
	}
	(&MasterClient{}).maybeLicenseReject(status.Error(codes.PermissionDenied, "license: X"))
}
