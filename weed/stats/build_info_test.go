package stats

import (
	"io"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MassDB Manager reads each process's version with this pattern (MassDbOssPlugin.BUILD_INFO_RE).
var managerBuildInfo = regexp.MustCompile(`(?m)^SeaweedFS_build_info\{[^}\r\n]*\bversion="([^"\r\n]{1,64})"[^}\r\n]*}\s+1(?:\.0)?\s*$`)

func TestSetBuildInfoIsReadableByMassDBManager(t *testing.T) {
	SetBuildInfo("3.84", "0430f659a", "30GB")
	SetBuildInfo("3.84", "41301715a", "30GB") // a second call replaces the series instead of adding one

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(Gather, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)

	all := managerBuildInfo.FindAllStringSubmatch(string(body), -1)
	if len(all) != 1 {
		t.Fatalf("want one SeaweedFS_build_info series, got %d in:\n%s", len(all), body)
	}
	if all[0][1] != "3.84" {
		t.Fatalf("version label = %q, want 3.84", all[0][1])
	}
	if !regexp.MustCompile(`commit="41301715a"`).MatchString(all[0][0]) {
		t.Fatalf("commit label missing in %q", all[0][0])
	}
}
