package license

// clockSuspectSlackSeconds is how far the wall clock may fall behind the
// persisted watermark before the master reports CLOCK_SUSPECT.
const clockSuspectSlackSeconds = 5

// Certs is the certificate part of the license state: Newest is the
// highest sequence ever accepted; Prev is the certificate that was in
// effect when Newest was imported, kept only while Newest has not started.
// Which one is in effect is a pure function of time, so masters and nodes
// never need an activation step.
type Certs struct {
	Newest *Certificate
	Prev   *Certificate
}

// Effective returns the certificate in effect at now, or nil.
func (s Certs) Effective(now int64) *Certificate {
	if s.Newest == nil {
		return nil
	}
	if now >= s.Newest.Claims.NotBefore {
		return s.Newest
	}
	return s.Prev
}

// Limits returns the node quotas in force at now. While Newest has not
// started the smaller of Prev and Newest applies, so a pending lower
// license is honoured early. Without any certificate every quota is 0:
// no node may join an unlicensed cluster.
func (s Certs) Limits(now int64) Limits {
	if s.Newest == nil {
		return Limits{}
	}
	if now >= s.Newest.Claims.NotBefore {
		return s.Newest.Claims.Limits
	}
	if s.Prev == nil {
		return s.Newest.Claims.Limits
	}
	return s.Prev.Claims.Limits.Min(s.Newest.Claims.Limits)
}

// Status evaluates the certificate part of the state at now.
func (s Certs) Status(now int64) Reason {
	if s.Newest == nil {
		return ReasonNoLicense
	}
	return certStatus(s.Effective(now), now)
}

func certStatus(c *Certificate, now int64) Reason {
	switch {
	case c == nil || now < c.Claims.NotBefore:
		return ReasonNotYetValid
	case now >= c.Claims.ExpiresAt:
		return ReasonExpired
	case !c.Claims.HasFeature(FeatureDataRead):
		return ReasonFeatureMissing
	default:
		return ReasonValid
	}
}

// Usage counts ledger entries per node type.
type Usage struct {
	Masters       int64
	VolumeServers int64
	Filers        int64
}

// ImportResult is the outcome of CheckImport.
type ImportResult struct {
	Certs Certs
	// Unchanged is true when the certificate is byte-identical to Newest;
	// an import retry only re-pushes the current state.
	Unchanged bool
	// Changes lists what the import takes away; empty, never nil.
	Changes []Change
}

// Change codes. A higher sequence replaces every earlier certificate, so
// shorter, smaller or later certificates import like any other; validate
// and import report what they take away, as MassDB SQL does (contract 1.5).
const (
	ChangeExpiryShortened          = "EXPIRY_SHORTENED"
	ChangeCoverageGap              = "COVERAGE_GAP"
	ChangeMasterLimitReduced       = "MASTER_LIMIT_REDUCED"
	ChangeVolumeServerLimitReduced = "VOLUME_SERVER_LIMIT_REDUCED"
	ChangeFilerLimitReduced        = "FILER_LIMIT_REDUCED"
	ChangePendingDiscarded         = "PENDING_DISCARDED"
	ChangeFeaturesRemoved          = "FEATURES_REMOVED"
)

// Change is one fact about an import: "code" and the fields of that code.
type Change map[string]interface{}

// CheckImport validates importing c into the current state on the master
// (contract 1.5 steps 2-6). Verification of c itself is done by
// ParseCertificate.
func (s Certs) CheckImport(c *Certificate, clusterID string, now int64, used Usage) (ImportResult, error) {
	if c.Claims.DeploymentID != clusterID {
		return ImportResult{}, errorf(CodeDeploymentMismatch, "certificate is for another deployment")
	}
	if c.Claims.ExpiresAt <= now {
		return ImportResult{}, errorf(CodeExpired, "certificate expired at %d", c.Claims.ExpiresAt)
	}
	if s.Newest != nil {
		switch {
		case c.SameBytes(s.Newest):
			return ImportResult{Certs: s, Unchanged: true, Changes: []Change{}}, nil
		case c.Claims.Sequence == s.Newest.Claims.Sequence:
			return ImportResult{}, errorf(CodeSequenceConflict, "sequence %d is already used by a different certificate", c.Claims.Sequence)
		case c.Claims.Sequence < s.Newest.Claims.Sequence:
			return ImportResult{}, errorf(CodeSequenceNotIncreased, "sequence %d is not above %d", c.Claims.Sequence, s.Newest.Claims.Sequence)
		}
	}
	// The new certificate must cover current usage on its own: once it
	// starts, its limits alone apply.
	l := c.Claims.Limits
	if l.MaxMasterNodes < used.Masters || l.MaxVolumeServers < used.VolumeServers || l.MaxFilers < used.Filers {
		return ImportResult{}, errorf(CodeNodeLimitTooSmall, "limits %d/%d/%d are below registered %d/%d/%d",
			l.MaxMasterNodes, l.MaxVolumeServers, l.MaxFilers, used.Masters, used.VolumeServers, used.Filers)
	}
	next := Certs{Newest: c}
	if c.Claims.NotBefore > now {
		next.Prev = s.Effective(now)
	}
	return ImportResult{Certs: next, Changes: s.changes(c, now)}, nil
}

// changes compares c with the certificate in effect at now and with a
// pending Newest that c replaces.
func (s Certs) changes(c *Certificate, now int64) []Change {
	changes := []Change{}
	n := c.Claims
	active := s.Effective(now)
	if active != nil {
		a := active.Claims
		if n.ExpiresAt < a.ExpiresAt {
			changes = append(changes, Change{"code": ChangeExpiryShortened, "previous_expires_at": a.ExpiresAt, "expires_at": n.ExpiresAt})
		}
		if n.NotBefore > now && n.NotBefore > a.ExpiresAt {
			changes = append(changes, Change{"code": ChangeCoverageGap, "gap_seconds": n.NotBefore - a.ExpiresAt})
		}
		for _, l := range []struct {
			code          string
			previous, now int64
		}{
			{ChangeMasterLimitReduced, a.Limits.MaxMasterNodes, n.Limits.MaxMasterNodes},
			{ChangeVolumeServerLimitReduced, a.Limits.MaxVolumeServers, n.Limits.MaxVolumeServers},
			{ChangeFilerLimitReduced, a.Limits.MaxFilers, n.Limits.MaxFilers},
		} {
			if l.now < l.previous {
				changes = append(changes, Change{"code": l.code, "previous_limit": l.previous, "limit": l.now})
			}
		}
	}
	if s.Newest != nil && now < s.Newest.Claims.NotBefore {
		changes = append(changes, Change{"code": ChangePendingDiscarded, "license_id": s.Newest.Claims.LicenseID, "sequence": s.Newest.Claims.Sequence})
	}
	if active != nil {
		var removed []string
		for _, f := range active.Claims.Features {
			if !c.Claims.HasFeature(f) {
				removed = append(removed, f)
			}
		}
		if len(removed) > 0 {
			changes = append(changes, Change{"code": ChangeFeaturesRemoved, "features": removed})
		}
	}
	return changes
}

// Compare decides whether incoming certificates replace the local ones
// when syncing between masters or from master to a node: a higher Newest
// sequence replaces both certificates together; an equal sequence must be
// byte-identical, and then incoming replaces the pair only to bring a
// better Prev (prevBetter); a lower one is refused.
func (s Certs) Compare(incoming Certs) (replace bool, err error) {
	switch {
	case incoming.Newest == nil:
		return false, nil
	case s.Newest == nil:
		return true, nil
	case incoming.Newest.Claims.Sequence > s.Newest.Claims.Sequence:
		return true, nil
	case incoming.Newest.Claims.Sequence < s.Newest.Claims.Sequence:
		return false, errorf(CodeSequenceNotIncreased, "incoming sequence %d is below local %d",
			incoming.Newest.Claims.Sequence, s.Newest.Claims.Sequence)
	case !incoming.Newest.SameBytes(s.Newest):
		return false, errorf(CodeSequenceConflict, "sequence %d differs between local and incoming", s.Newest.Claims.Sequence)
	default:
		return prevBetter(incoming.Prev, s.Prev), nil
	}
}

// prevBetter reports whether a should replace b as Prev under the same
// Newest. Masters that applied the same future certificate from different
// states (e.g. a lagging master started with -license.file) hold different
// Prev certificates; all converge on the highest sequence, any over none,
// and equal sequences with different bytes are ordered by their bytes, so
// any two states agree on which one wins.
func prevBetter(a, b *Certificate) bool {
	switch {
	case a == nil || a.SameBytes(b):
		return false
	case b == nil:
		return true
	case a.Claims.Sequence != b.Claims.Sequence:
		return a.Claims.Sequence > b.Claims.Sequence
	default:
		return a.raw > b.raw
	}
}

// ClockState is the master clock-rollback state (plan 4.5).
type ClockState struct {
	Watermark int64 `json:"watermark"`
	RepairSeq int64 `json:"repair_seq"`
}

// Suspect reports whether the wall clock has fallen behind the watermark.
func (c ClockState) Suspect(now int64) bool {
	return now < c.Watermark-clockSuspectSlackSeconds
}

// Advance raises the watermark to now; it never lowers it.
func (c ClockState) Advance(now int64) ClockState {
	if now > c.Watermark {
		c.Watermark = now
	}
	return c
}

// RepairOnImport is applied when a certificate with a higher sequence is
// imported: if it was issued no later than now, its issue time becomes the
// watermark, which may lower it and so clear CLOCK_SUSPECT.
func (c ClockState) RepairOnImport(imported *Certificate, now int64) ClockState {
	if imported.Claims.IssuedAt <= now && imported.Claims.Sequence > c.RepairSeq {
		return ClockState{Watermark: imported.Claims.IssuedAt, RepairSeq: imported.Claims.Sequence}
	}
	return c
}

// Merge applies a peer's clock state: a newer repair wins outright, even
// when it lowers the watermark; the same repair keeps the higher
// watermark; an older repair is ignored so a late request cannot raise the
// watermark again. The peer's watermark counts at most as this master's
// now first: CLOCK_SUSPECT is about this master's clock falling behind its
// own past, and a peer whose clock runs ahead is a skew, not a rollback
// (plan 4.5, revised 2026-10-25).
func (c ClockState) Merge(in ClockState, now int64) ClockState {
	if in.Watermark > now {
		in.Watermark = now
	}
	switch {
	case in.RepairSeq > c.RepairSeq:
		return in
	case in.RepairSeq == c.RepairSeq:
		return c.Advance(in.Watermark)
	default:
		return c
	}
}
