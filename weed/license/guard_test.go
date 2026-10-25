package license

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func guardWith(reason Reason, cert *Certificate, now int64) *Guard {
	g := newGuard()
	g.snap.Store(newSnapshot(reason, cert, testDeployment))
	g.clock.Store(now)
	return g
}

func TestGuardDecisions(t *testing.T) {
	valid := certWith(t, 1, t2026, t2027, FeatureDataRead)
	noFeature := certWith(t, 1, t2026, t2027)
	cases := []struct {
		name   string
		reason Reason
		cert   *Certificate
		now    int64
		allow  bool
		deny   Reason
	}{
		{"not ready", ReasonNotReady, nil, t2026, false, ReasonNotReady},
		{"valid first second", "", valid, t2026, true, ""},
		{"valid last second", "", valid, t2027 - 1, true, ""},
		{"expiry second", "", valid, t2027, false, ReasonExpired},
		{"before start", "", valid, t2026 - 1, false, ReasonNotYetValid},
		{"no feature", "", noFeature, t2026 + 1, false, ReasonFeatureMissing},
		{"no feature and expired", "", noFeature, t2027, false, ReasonExpired},
		{"clock suspect", ReasonClockSuspect, valid, t2026 + 1, false, ReasonClockSuspect},
	}
	for _, c := range cases {
		g := guardWith(c.reason, c.cert, c.now)
		ok, s := g.AllowRead()
		if ok != c.allow {
			t.Errorf("%s: allow = %v", c.name, ok)
			continue
		}
		if !ok && s.Reason(c.now) != c.deny {
			t.Errorf("%s: reason = %s, want %s", c.name, s.Reason(c.now), c.deny)
		}
	}
}

func TestWriteDenied(t *testing.T) {
	cert := certWith(t, 1, t2026, t2027, FeatureDataRead)
	g := guardWith("", cert, t2027)
	ok, s := g.AllowRead()
	if ok {
		t.Fatal("expired certificate allowed")
	}
	w := httptest.NewRecorder()
	g.WriteDenied(w, s)
	if w.Code != 403 || w.Header().Get(ReasonHeader) != string(ReasonExpired) || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	want := `{"error":"license_denied","reason":"EXPIRED","expires_at":1798761600,"cluster_id":"` + testDeployment + `"}`
	if w.Body.String() != want {
		t.Fatalf("body %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	g = guardWith(ReasonClockSuspect, cert, t2026+1)
	_, s = g.AllowRead()
	g.WriteDenied(w, s)
	if want := `{"error":"license_denied","reason":"CLOCK_SUSPECT","expires_at":1798761600,"cluster_id":"` + testDeployment + `"}`; w.Body.String() != want {
		t.Fatalf("clock suspect body %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	g = guardWith(ReasonNoLicense, nil, t2026)
	_, s = g.AllowRead()
	g.WriteDenied(w, s)
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["reason"] != "NO_LICENSE" || body["expires_at"] != float64(0) {
		t.Fatalf("no license body %s", w.Body.String())
	}
}

func TestInternalToken(t *testing.T) {
	key := bytes.Repeat([]byte{7}, InternalKeySize)
	token := EncodeInternalToken(key)
	g := guardWith(ReasonNoLicense, nil, t2026)
	if g.InternalTokenValid(token) {
		t.Fatal("token accepted before a key is set")
	}
	g.setKey(key)
	if !g.InternalTokenValid(token) {
		t.Fatal("valid token rejected")
	}
	other := EncodeInternalToken(bytes.Repeat([]byte{8}, InternalKeySize))
	for name, v := range map[string]string{
		"empty":        "",
		"wrong key":    other,
		"short":        EncodeInternalToken(key[:31]),
		"long":         EncodeInternalToken(append(key, 0)),
		"not base64":   "!" + token[1:],
		"url alphabet": "-" + token[1:],
	} {
		if g.InternalTokenValid(v) {
			t.Errorf("%s token accepted", name)
		}
	}
	g.setKey(key[:16])
	if g.InternalTokenValid(token) {
		t.Error("a wrong-length key must leave the token disabled")
	}
	g = guardWith(ReasonNotReady, nil, t2026)
	g.setKey(key)
	if g.InternalTokenValid(token) {
		t.Error("NOT_READY must reject every token")
	}
}

func TestGuardAllowReadNoAlloc(t *testing.T) {
	cert := certWith(t, 1, t2026, t2027, FeatureDataRead)
	for _, now := range []int64{t2026 + 1, t2027} {
		g := guardWith("", cert, now)
		if n := testing.AllocsPerRun(1000, func() { g.AllowRead() }); n != 0 {
			t.Errorf("AllowRead at %d allocates %v times", now, n)
		}
	}
}

func BenchmarkGuardAllowRead(b *testing.B) {
	g := guardWith("", certWith(b, 1, t2026, t2027, FeatureDataRead), t2026+1)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if ok, _ := g.AllowRead(); !ok {
				b.Fatal("denied")
			}
		}
	})
}

// WriteDeniedStatus gives an unauthenticated caller the reason only.
func TestWriteDeniedStatus(t *testing.T) {
	cert := certWith(t, 1, t2026, t2027, FeatureDataRead)
	g := guardWith("", cert, t2027)
	_, s := g.AllowRead()
	w := httptest.NewRecorder()
	g.WriteDeniedStatus(w, s)
	if w.Code != 403 || w.Header().Get(ReasonHeader) != string(ReasonExpired) {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" || w.Header().Get("Content-Length") != "" {
		t.Fatalf("body %q headers %v", w.Body.String(), w.Header())
	}
}
