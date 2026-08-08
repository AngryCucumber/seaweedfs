package massdblicense

import (
	"errors"
	"strings"
	"testing"
)

func TestCorrectionBarrierBlocksEquivalentPreCorrectionFile(t *testing.T) {
	state := initializedWithLicense(t, 2_000)
	shorter := license("short", strings.Repeat("c", 64), 200, 1_500)
	prepared, err := state.PrepareLicense("op-2", "key-2", strings.Repeat("d", 64),
		IntentReplaceWithShorter, shorter, "requester", "approver", 400, 500)
	if err != nil {
		t.Fatal(err)
	}
	state, err = prepared.Commit("op-2", 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.LicenseCorrectionBarriers) != 1 {
		t.Fatalf("barriers=%d", len(state.LicenseCorrectionBarriers))
	}

	_, err = state.PrepareLicense("op-3", "key-3", strings.Repeat("e", 64),
		IntentNormal, license("alternate", strings.Repeat("f", 64), 300, 2_000),
		"tenant-admin", "", 410, 500)
	requireCode(t, err, "MASSDB_LICENSE_SUPERSEDED")

	issuedAt := state.LicenseCorrectionBarriers[0].SupersededIssuedAtCutoff + 1
	_, err = state.PrepareLicense("op-4", "key-4", strings.Repeat("1", 64),
		IntentNormal, license("new", strings.Repeat("2", 64), issuedAt, 2_000),
		"tenant-admin", "", 410, 500)
	if err != nil {
		t.Fatalf("newly issued replacement should pass: %v", err)
	}
}

func TestRestrictiveKeysetPreemptsChallengeOnlyOnCommit(t *testing.T) {
	state := initializedWithLicense(t, 2_000)
	state, err := state.CreateClockChallenge("challenge", strings.Repeat("3", 64), 300, 900)
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.PrepareKeyset("add", "key-add", strings.Repeat("4", 64),
		MutationAdditiveKeyset, keyset(2, "4"), 301, 500)
	requireCode(t, err, "MASSDB_LICENSE_CLOCK_RECOVERY_CHALLENGE_ACTIVE")

	prepared, err := state.PrepareKeyset("recover", "key-recover", strings.Repeat("5", 64),
		MutationRestrictiveKeyset, keyset(2, "5"), 301, 500)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ClockChallenge.State != ChallengeActive {
		t.Fatalf("prepare changed challenge: %s", prepared.ClockChallenge.State)
	}
	committed, err := prepared.Commit("recover", 302)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ClockChallenge.State != ChallengeInvalidatedByKeysetRecovery {
		t.Fatalf("challenge=%s", committed.ClockChallenge.State)
	}
}

func TestRecoveryBundleAtomicallyCommitsKeysetAndLicense(t *testing.T) {
	state := initializedWithLicense(t, 2_000)
	challenged, err := state.CreateClockChallenge("challenge", strings.Repeat("3", 64), 300, 900)
	if err != nil {
		t.Fatal(err)
	}
	replacement := license("replacement", strings.Repeat("a", 64), 250, 2_500)
	prepared, err := challenged.PrepareRecoveryBundle("bundle", "key-bundle", strings.Repeat("f", 64),
		keyset(3, "e"), replacement, 301, 500)
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := prepared.Abort("bundle")
	if err != nil {
		t.Fatal(err)
	}
	if aborted.ActiveKeyset != nil || aborted.ActiveLicense.LicenseID != "initial" ||
		aborted.ClockChallenge.State != ChallengeActive {
		t.Fatalf("abort changed authoritative state: %+v", aborted)
	}

	prepared, err = challenged.PrepareRecoveryBundle("bundle-2", "key-bundle-2", strings.Repeat("9", 64),
		keyset(3, "d"), replacement, 301, 500)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := prepared.Commit("bundle-2", 302)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ActiveKeyset == nil || committed.ActiveKeyset.Version != 3 ||
		committed.ActiveLicense.LicenseID != "replacement" ||
		committed.ClockChallenge.State != ChallengeInvalidatedByKeysetRecovery {
		t.Fatalf("bundle was not committed atomically: %+v", committed)
	}
}

func TestIdempotencyAndMutationSlot(t *testing.T) {
	state := initializedWithLicense(t, 2_000)
	prepared, err := state.PrepareLicense("op", "idem", strings.Repeat("6", 64),
		IntentNormal, license("next", strings.Repeat("7", 64), 200, 3_000),
		"admin", "", 300, 500)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := prepared.PrepareLicense("ignored", "idem", strings.Repeat("6", 64),
		IntentNormal, license("other", strings.Repeat("8", 64), 200, 4_000),
		"admin", "", 301, 501)
	if err != nil || replayed.Mutation.OperationID != "op" {
		t.Fatalf("idempotent replay failed: %+v %v", replayed.Mutation, err)
	}
	_, err = prepared.PrepareLicense("other", "different", strings.Repeat("9", 64),
		IntentNormal, license("other", strings.Repeat("a", 64), 200, 4_000),
		"admin", "", 301, 501)
	requireCode(t, err, "MASSDB_LICENSE_MUTATION_IN_PROGRESS")
}

func TestStoreRestoreRetainsBarrier(t *testing.T) {
	state := initializedWithLicense(t, 2_000)
	prepared, _ := state.PrepareLicense("op-2", "key-2", strings.Repeat("b", 64),
		IntentReplaceWithShorter, license("short", strings.Repeat("c", 64), 200, 1_500),
		"requester", "approver", 400, 500)
	state, _ = prepared.Commit("op-2", 400)
	store := NewStore()
	if err := store.Restore(state); err != nil {
		t.Fatal(err)
	}
	restored := store.Snapshot()
	if len(restored.LicenseCorrectionBarriers) != 1 ||
		restored.LicenseCorrectionBarriers[0].SupersededExpiresAt != 2_000 {
		t.Fatalf("barrier lost: %+v", restored.LicenseCorrectionBarriers)
	}
}

func initializedWithLicense(t *testing.T, expiresAt uint64) State {
	t.Helper()
	state, err := EmptyState().Bootstrap(false, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := state.PrepareLicense("op-1", "key-1", strings.Repeat("b", 64),
		IntentNormal, license("initial", strings.Repeat("0", 64), 100, expiresAt),
		"admin", "", 200, 300)
	if err != nil {
		t.Fatal(err)
	}
	state, err = prepared.Commit("op-1", 201)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func license(id, sha string, issuedAt, expiresAt uint64) ActiveLicense {
	return ActiveLicense{LicenseID: id, SHA256: sha, Kid: "license-kid", IssuedAt: issuedAt,
		ExpiresAt: expiresAt, Artifact: []byte{1, 2, 3}}
}

func keyset(version uint64, shaCharacter string) ActiveKeyset {
	return ActiveKeyset{Version: version, SHA256: strings.Repeat(shaCharacter, 64),
		Artifact: []byte{4, 5, 6}}
}

func requireCode(t *testing.T, err error, expected string) {
	t.Helper()
	var licenseErr *Error
	if !errors.As(err, &licenseErr) || licenseErr.Code != expected {
		t.Fatalf("expected %s, got %v", expected, err)
	}
}
