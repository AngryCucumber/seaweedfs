package license

import "testing"

func TestEffectiveAndStatusBoundaries(t *testing.T) {
	c := certWith(t, 1, t2026, t2027, FeatureDataRead)
	s := Certs{Newest: c}
	cases := []struct {
		now  int64
		want Reason
	}{
		{t2026 - 1, ReasonNotYetValid},
		{t2026, ReasonValid},
		{t2027 - 1, ReasonValid},
		{t2027, ReasonExpired}, // expiry second itself is expired
	}
	for _, tc := range cases {
		if got := s.Status(tc.now); got != tc.want {
			t.Errorf("Status(%d) = %s, want %s", tc.now, got, tc.want)
		}
	}
	if (Certs{}).Status(t2026) != ReasonNoLicense {
		t.Error("empty state must be NO_LICENSE")
	}
	noFeature := Certs{Newest: certWith(t, 1, t2026, t2027)}
	if noFeature.Status(t2026) != ReasonFeatureMissing || noFeature.Status(t2027) != ReasonExpired {
		t.Error("feature check must come after the time check")
	}
}

func TestEffectiveSwitchesAtNotBefore(t *testing.T) {
	prev := certWith(t, 10, t2026, t2027, FeatureDataRead)
	newest := certWith(t, 12, t2027-100, t2027+1000, FeatureDataRead)
	s := Certs{Newest: newest, Prev: prev}
	if s.Effective(t2027-101) != prev || s.Effective(t2027-100) != newest {
		t.Fatal("effective certificate must switch exactly at newest.not_before")
	}
	if (Certs{Newest: newest}).Effective(t2027-101) != nil {
		t.Fatal("pending newest without prev has no effective certificate")
	}
}

func TestLimits(t *testing.T) {
	if (Certs{}).Limits(t2026) != (Limits{}) {
		t.Error("an unlicensed cluster admits no node")
	}
	prev := mustCert(t, func(c map[string]interface{}) {
		c["sequence"] = 1
		c["limits"] = map[string]interface{}{"max_master_nodes": 3, "max_volume_servers": 10, "max_filers": 1}
	})
	newest := mustCert(t, func(c map[string]interface{}) {
		c["sequence"], c["not_before"], c["expires_at"] = 2, t2027-10, t2027+10
		c["limits"] = map[string]interface{}{"max_master_nodes": 5, "max_volume_servers": 4, "max_filers": 2}
	})
	s := Certs{Newest: newest, Prev: prev}
	if got := s.Limits(t2026); got != (Limits{3, 4, 1}) {
		t.Errorf("pending: got %+v, want the smaller of both", got)
	}
	if got := s.Limits(t2027 - 10); got != newest.Claims.Limits {
		t.Errorf("started: got %+v", got)
	}
	if got := (Certs{Newest: newest}).Limits(t2026); got != newest.Claims.Limits {
		t.Errorf("pending without prev: got %+v", got)
	}
	expired := Certs{Newest: prev}
	if got := expired.Limits(t2027 + 5); got != prev.Claims.Limits {
		t.Errorf("expired certificate keeps constraining joins: got %+v", got)
	}
}

func TestCheckImport(t *testing.T) {
	now := int64(t2026 + 100)
	c10 := certWith(t, 10, t2026, t2027, FeatureDataRead)
	s := Certs{Newest: c10}
	used := Usage{Masters: 3, VolumeServers: 7, Filers: 2}

	if _, err := s.CheckImport(c10, "00000000-0000-4000-8000-000000000000", now, used); codeOf(err) != CodeDeploymentMismatch {
		t.Errorf("deployment: %v", err)
	}
	expired := certWith(t, 11, t2026-10, t2026+50, FeatureDataRead)
	if _, err := s.CheckImport(expired, testDeployment, now, used); codeOf(err) != CodeExpired {
		t.Errorf("expired: %v", err)
	}
	if r, err := s.CheckImport(c10, testDeployment, now, used); err != nil || !r.Unchanged || r.Certs.Newest != c10 {
		t.Errorf("same bytes must be an idempotent retry: %+v %v", r, err)
	}
	conflict := certWith(t, 10, t2026, t2027-1, FeatureDataRead)
	if _, err := s.CheckImport(conflict, testDeployment, now, used); codeOf(err) != CodeSequenceConflict {
		t.Errorf("same sequence, other bytes: %v", err)
	}
	lower := certWith(t, 9, t2026, t2027, FeatureDataRead)
	if _, err := s.CheckImport(lower, testDeployment, now, used); codeOf(err) != CodeSequenceNotIncreased {
		t.Errorf("lower sequence: %v", err)
	}
	small := mustCert(t, func(c map[string]interface{}) {
		c["sequence"] = 11
		c["limits"] = map[string]interface{}{"max_master_nodes": 3, "max_volume_servers": 6, "max_filers": 2}
	})
	if _, err := s.CheckImport(small, testDeployment, now, used); codeOf(err) != CodeNodeLimitTooSmall {
		t.Errorf("limits below usage: %v", err)
	}
	noFeature := certWith(t, 11, t2026, t2027)
	if _, err := s.CheckImport(noFeature, testDeployment, now, Usage{}); err != nil {
		t.Errorf("a certificate without DATA_READ still imports: %v", err)
	}

	immediate := certWith(t, 11, t2026, t2027+100, FeatureDataRead)
	if r, err := s.CheckImport(immediate, testDeployment, now, used); err != nil || r.Certs.Newest != immediate || r.Certs.Prev != nil {
		t.Errorf("immediate: %+v %v", r, err)
	}
	future := certWith(t, 12, now+50, t2027+100, FeatureDataRead)
	r, err := s.CheckImport(future, testDeployment, now, used)
	if err != nil || r.Certs.Newest != future || r.Certs.Prev != c10 {
		t.Fatalf("future: %+v %v", r, err)
	}
	// Importing 13 over (12 pending, 10) keeps the certificate in effect now (10).
	next := certWith(t, 13, now+80, t2027+200, FeatureDataRead)
	if r2, err := r.Certs.CheckImport(next, testDeployment, now, used); err != nil || r2.Certs.Prev != c10 {
		t.Errorf("chained future import: %+v %v", r2, err)
	}
	if r3, err := (Certs{}).CheckImport(future, testDeployment, now, Usage{}); err != nil || r3.Certs.Prev != nil {
		t.Errorf("first import of a future certificate: %+v %v", r3, err)
	}
}

func TestCompare(t *testing.T) {
	c10 := certWith(t, 10, t2026, t2027, FeatureDataRead)
	c11 := certWith(t, 11, t2026, t2027, FeatureDataRead)
	c12 := certWith(t, 12, t2027-1, t2027+10, FeatureDataRead)
	other12 := certWith(t, 12, t2027-2, t2027+10, FeatureDataRead)

	// Review case: majority holds (12, 10); a node that took 11 as its
	// newest must be replaced wholesale by (12, 10).
	local := Certs{Newest: c11, Prev: c10}
	if replace, err := local.Compare(Certs{Newest: c12, Prev: c10}); !replace || err != nil {
		t.Errorf("higher sequence must replace: %v %v", replace, err)
	}
	if replace, err := (Certs{Newest: c12}).Compare(Certs{Newest: c11}); replace || codeOf(err) != CodeSequenceNotIncreased {
		t.Errorf("lower sequence must be refused: %v %v", replace, err)
	}
	if replace, err := (Certs{Newest: c12}).Compare(Certs{Newest: other12}); replace || codeOf(err) != CodeSequenceConflict {
		t.Errorf("same sequence, other bytes: %v %v", replace, err)
	}
	if replace, err := (Certs{Newest: c12}).Compare(Certs{Newest: c12}); replace || err != nil {
		t.Errorf("identical: %v %v", replace, err)
	}
	if replace, err := (Certs{Newest: c12}).Compare(Certs{}); replace || err != nil {
		t.Errorf("empty incoming keeps local: %v %v", replace, err)
	}
	if replace, err := (Certs{}).Compare(Certs{Newest: c10}); !replace || err != nil {
		t.Errorf("empty local takes incoming: %v %v", replace, err)
	}
}

func TestClockState(t *testing.T) {
	c := ClockState{Watermark: 1000}
	if c.Suspect(995) || !c.Suspect(994) {
		t.Error("suspect only when more than 5s behind the watermark")
	}
	if c.Advance(900).Watermark != 1000 || c.Advance(1100).Watermark != 1100 {
		t.Error("Advance never lowers the watermark")
	}

	cert := certWith(t, 7, t2026, t2027, FeatureDataRead)
	rolled := ClockState{Watermark: t2026 + 10000, RepairSeq: 3}
	repaired := rolled.RepairOnImport(cert, t2026+10)
	if repaired.Watermark != t2026 || repaired.RepairSeq != 7 || repaired.Suspect(t2026+10) {
		t.Errorf("import of a newer certificate issued before now repairs the clock: %+v", repaired)
	}
	if got := rolled.RepairOnImport(cert, t2026-1); got != rolled {
		t.Errorf("certificate issued in the future must not repair: %+v", got)
	}

	local := ClockState{Watermark: 5000, RepairSeq: 7}
	if got := local.Merge(ClockState{Watermark: 100, RepairSeq: 8}); got.Watermark != 100 || got.RepairSeq != 8 {
		t.Errorf("newer repair wins even when it lowers the watermark: %+v", got)
	}
	if got := local.Merge(ClockState{Watermark: 6000, RepairSeq: 7}); got.Watermark != 6000 {
		t.Errorf("same repair keeps the higher watermark: %+v", got)
	}
	if got := local.Merge(ClockState{Watermark: 9000, RepairSeq: 6}); got != local {
		t.Errorf("late older repair must not raise the watermark: %+v", got)
	}
}
