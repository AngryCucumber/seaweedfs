package weed_server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/license/licensetest"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

func TestLicenseHttpReadAllowed(t *testing.T) {
	valid, denied, notReady := licensetest.ValidNode(t), licensetest.DeniedNode(t), licensetest.NotReadyNode(t)
	cases := []struct {
		name        string
		node        *license.Node
		token       string
		acceptToken bool
		allowed     bool
		reason      license.Reason
	}{
		{"valid", valid, "", true, true, ""},
		{"no license", denied, "", true, false, license.ReasonNoLicense},
		{"internal token", denied, licensetest.Token(), true, true, ""},
		{"token not accepted", denied, licensetest.Token(), false, false, license.ReasonNoLicense},
		{"wrong token", denied, license.EncodeInternalToken(make([]byte, license.InternalKeySize)), true, false, license.ReasonNoLicense},
		{"short token", denied, license.EncodeInternalToken([]byte("short")), true, false, license.ReasonNoLicense},
		{"not ready", notReady, licensetest.Token(), true, false, license.ReasonNotReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/3,01637037d6", nil)
			if c.token != "" {
				r.Header.Set(license.InternalReadHeader, c.token)
			}
			w := httptest.NewRecorder()
			if got := licenseHttpReadAllowed(c.node, w, r, "volumeServer", licensePathVolumeHttp, c.acceptToken); got != c.allowed {
				t.Fatalf("allowed = %v, want %v", got, c.allowed)
			}
			if c.allowed {
				if w.Code != http.StatusOK || w.Body.Len() != 0 {
					t.Fatalf("allowed read wrote %d %q", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusForbidden || w.Header().Get(license.ReasonHeader) != string(c.reason) {
				t.Fatalf("denial: %d %q", w.Code, w.Header().Get(license.ReasonHeader))
			}
			var body map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "license_denied" || body["reason"] != string(c.reason) {
				t.Fatalf("denial body %q: %v", w.Body.String(), err)
			}
		})
	}
}

func TestLicenseGrpcReadAllowed(t *testing.T) {
	valid, denied := licensetest.ValidNode(t), licensetest.DeniedNode(t)
	if err := licenseGrpcReadAllowed(valid, context.Background()); err != nil {
		t.Fatalf("valid: %v", err)
	}
	err := licenseGrpcReadAllowed(denied, context.Background())
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "license: NO_LICENSE" {
		t.Fatalf("denied: %v", err)
	}
	withToken := metadata.NewIncomingContext(context.Background(), metadata.Pairs(license.InternalReadMetadataKey, licensetest.Token()))
	if err := licenseGrpcReadAllowed(denied, withToken); err != nil {
		t.Fatalf("token: %v", err)
	}
	twoTokens := metadata.NewIncomingContext(context.Background(), metadata.Pairs(license.InternalReadMetadataKey, licensetest.Token(), license.InternalReadMetadataKey, licensetest.Token()))
	if err := licenseGrpcReadAllowed(denied, twoTokens); err == nil {
		t.Fatal("accepted a repeated token")
	}

	// Volume to volume calls carry the token the receiver accepts.
	out := licenseOutgoingContext(denied, context.Background())
	md, _ := metadata.FromOutgoingContext(out)
	if err := licenseGrpcReadAllowed(denied, metadata.NewIncomingContext(context.Background(), md)); err != nil {
		t.Fatalf("outgoing token: %v", err)
	}
	if out := licenseOutgoingContext(licensetest.NotReadyNode(t), context.Background()); out != context.Background() {
		t.Fatal("a node without key added a token")
	}
}

func TestIsCopyFileMetadataExt(t *testing.T) {
	for _, ext := range []string{".idx", ".ecx", ".ecj", ".vif"} {
		if !isCopyFileMetadataExt(ext) {
			t.Errorf("%s should be metadata", ext)
		}
	}
	for _, ext := range []string{".dat", ".ec00", ".ec13", ".cpx", ""} {
		if isCopyFileMetadataExt(ext) {
			t.Errorf("%s should be guarded", ext)
		}
	}
}

func TestLicenseUpdateFromPb(t *testing.T) {
	u := licenseUpdateFromPb(&master_pb.LicenseUpdate{
		NewestCertificate: "n", PrevCertificate: "p", MasterNowUnixMs: 5, ClusterId: "c",
		MasterDenyReason: "CLOCK_SUSPECT", InternalReadKey: []byte("k"), StateVersion: 7,
	})
	want := license.Update{NewestCertificate: "n", PrevCertificate: "p", MasterNowUnixMs: 5, ClusterID: "c",
		MasterDenyReason: "CLOCK_SUSPECT", InternalReadKey: []byte("k"), StateVersion: 7}
	if u.NewestCertificate != want.NewestCertificate || u.PrevCertificate != want.PrevCertificate ||
		u.MasterNowUnixMs != want.MasterNowUnixMs || u.ClusterID != want.ClusterID ||
		u.MasterDenyReason != want.MasterDenyReason || string(u.InternalReadKey) != "k" || u.StateVersion != 7 {
		t.Fatalf("got %+v", u)
	}
}
