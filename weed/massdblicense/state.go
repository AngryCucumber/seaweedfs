package massdblicense

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	"github.com/google/uuid"
)

const (
	MaxIdempotencyKeyBytes = 191
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type EnforcementMode string

const (
	ModeUninitialized EnforcementMode = "UNINITIALIZED"
	ModeObserve       EnforcementMode = "OBSERVE"
	ModeEnforcing     EnforcementMode = "ENFORCING"
)

type ImportIntent string

const (
	IntentNormal                 ImportIntent = "NORMAL"
	IntentReplaceWithShorter     ImportIntent = "REPLACE_WITH_SHORTER"
	IntentKeyRotationReplacement ImportIntent = "KEY_ROTATION_REPLACEMENT"
)

type MutationKind string

const (
	MutationLicense                     MutationKind = "LICENSE"
	MutationAdditiveKeyset              MutationKind = "ADDITIVE_KEYSET"
	MutationRestrictiveKeyset           MutationKind = "RESTRICTIVE_KEYSET"
	MutationKeysetLicenseRecoveryBundle MutationKind = "KEYSET_LICENSE_RECOVERY_BUNDLE"
	MutationClockRecovery               MutationKind = "CLOCK_RECOVERY"
)

type OperationState string

const (
	OperationPrepared  OperationState = "PREPARED"
	OperationSucceeded OperationState = "SUCCEEDED"
	OperationAborted   OperationState = "ABORTED"
)

type ClockChallengeState string

const (
	ChallengeActive                      ClockChallengeState = "ACTIVE"
	ChallengeCancelled                   ClockChallengeState = "CANCELLED"
	ChallengeConsumed                    ClockChallengeState = "CONSUMED"
	ChallengeInvalidatedByKeysetRecovery ClockChallengeState = "INVALIDATED_BY_KEYSET_RECOVERY"
)

type ActiveLicense struct {
	LicenseID string `json:"licenseId"`
	SHA256    string `json:"sha256"`
	Kid       string `json:"kid"`
	IssuedAt  uint64 `json:"issuedAt"`
	ExpiresAt uint64 `json:"expiresAt"`
	Artifact  []byte `json:"artifact"`
}

type ActiveKeyset struct {
	Version  uint64 `json:"version"`
	SHA256   string `json:"sha256"`
	Artifact []byte `json:"artifact"`
}

type CorrectionBarrier struct {
	SupersededExpiresAt      uint64 `json:"supersededExpiresAt"`
	SupersededIssuedAtCutoff uint64 `json:"supersededIssuedAtCutoff"`
	CorrectedAt              uint64 `json:"correctedAt"`
	SupersededLicenseID      string `json:"supersededLicenseId"`
	SupersededSHA256         string `json:"supersededSha256"`
}

type ClockChallenge struct {
	ChallengeID   string              `json:"challengeId"`
	ChallengeHex  string              `json:"challengeHex"`
	CreatedAt     uint64              `json:"createdAt"`
	ExpiresAt     uint64              `json:"expiresAt"`
	State         ClockChallengeState `json:"state"`
	InvalidatedAt *uint64             `json:"invalidatedAt,omitempty"`
}

func (c *ClockChallenge) active(now uint64) bool {
	return c != nil && c.State == ChallengeActive && now < c.ExpiresAt
}

type Mutation struct {
	OperationID      string         `json:"operationId"`
	Kind             MutationKind   `json:"kind"`
	Intent           ImportIntent   `json:"intent,omitempty"`
	IdempotencyKey   string         `json:"idempotencyKey"`
	RequestHash      string         `json:"requestHash"`
	Requester        string         `json:"requester,omitempty"`
	Approver         string         `json:"approver,omitempty"`
	PreparedAt       uint64         `json:"preparedAt"`
	DeadlineAt       uint64         `json:"deadlineAt"`
	CandidateLicense *ActiveLicense `json:"candidateLicense,omitempty"`
	CandidateKeyset  *ActiveKeyset  `json:"candidateKeyset,omitempty"`
	State            OperationState `json:"state"`
}

type IdempotencyRecord struct {
	RequestHash string         `json:"requestHash"`
	OperationID string         `json:"operationId"`
	State       OperationState `json:"state"`
}

// State is persisted in both supported Master Raft implementations and their snapshots.
type State struct {
	FormatVersion                int                          `json:"formatVersion"`
	LicenseControlDeploymentUUID string                       `json:"licenseControlDeploymentUuid,omitempty"`
	EnforcementMode              EnforcementMode              `json:"enforcementMode"`
	EnforcementEpoch             uint64                       `json:"enforcementEpoch"`
	TopologyRevision             uint64                       `json:"topologyRevision"`
	ActiveKeyset                 *ActiveKeyset                `json:"activeKeyset,omitempty"`
	ActiveLicense                *ActiveLicense               `json:"activeLicense,omitempty"`
	Mutation                     *Mutation                    `json:"mutation,omitempty"`
	LastOperation                *Mutation                    `json:"lastOperation,omitempty"`
	Idempotency                  map[string]IdempotencyRecord `json:"idempotency"`
	LicenseCorrectionBarriers    []CorrectionBarrier          `json:"licenseCorrectionBarriers"`
	ClockChallenge               *ClockChallenge              `json:"clockChallenge,omitempty"`
	ClockRecoveryEpoch           uint64                       `json:"clockRecoveryEpoch"`
	MaxAcceptedRecoverySequence  uint64                       `json:"maxAcceptedRecoverySequence"`
	BootstrapPhase               string                       `json:"bootstrapPhase"`
	BootstrapSealGeneration      uint64                       `json:"bootstrapSealGeneration"`
	BootstrapPlanSHA256          string                       `json:"bootstrapPlanSha256,omitempty"`
}

func EmptyState() State {
	return State{
		FormatVersion:             int(FormatVersion),
		EnforcementMode:           ModeUninitialized,
		Idempotency:               make(map[string]IdempotencyRecord),
		LicenseCorrectionBarriers: make([]CorrectionBarrier, 0),
		BootstrapPhase:            "UNINITIALIZED",
	}
}

func (s State) Clone() State {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var cloned State
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		panic(err)
	}
	cloned.normalize()
	return cloned
}

func (s *State) normalize() {
	if s.FormatVersion == 0 {
		s.FormatVersion = int(FormatVersion)
	}
	if s.Idempotency == nil {
		s.Idempotency = make(map[string]IdempotencyRecord)
	}
	if s.LicenseCorrectionBarriers == nil {
		s.LicenseCorrectionBarriers = make([]CorrectionBarrier, 0)
	}
}

func (s State) Bootstrap(existingMetadata bool, planSHA256 string) (State, error) {
	if s.EnforcementMode != ModeUninitialized {
		if s.BootstrapPlanSHA256 == planSHA256 {
			return s.Clone(), nil
		}
		return State{}, failure("MASSDB_LICENSE_BOOTSTRAP_SEALED", "License bootstrap已经SEALED")
	}
	if !sha256Pattern.MatchString(planSHA256) {
		return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "bootstrap plan SHA-256格式错误")
	}
	next := s.Clone()
	next.LicenseControlDeploymentUUID = uuid.NewString()
	if existingMetadata {
		next.EnforcementMode = ModeObserve
	} else {
		next.EnforcementMode = ModeEnforcing
	}
	next.BootstrapPhase = "SEALED"
	next.BootstrapSealGeneration = 1
	next.BootstrapPlanSHA256 = planSHA256
	return next, nil
}

func (s State) PrepareLicense(operationID, idempotencyKey, requestHash string,
	intent ImportIntent, candidate ActiveLicense, requester, approver string,
	now, deadlineAt uint64) (State, error) {
	if err := s.requireInitialized(); err != nil {
		return State{}, err
	}
	if err := validateOperation(operationID, idempotencyKey, requestHash, now, deadlineAt); err != nil {
		return State{}, err
	}
	if replay, found, err := s.replayIdempotency(idempotencyKey, requestHash); found || err != nil {
		return replay, err
	}
	if s.Mutation != nil {
		return State{}, failure("MASSDB_LICENSE_MUTATION_IN_PROGRESS", "已有License mutation占用统一槽位")
	}
	if err := validateLicense(candidate); err != nil {
		return State{}, err
	}
	if s.ClockChallenge.active(now) {
		return State{}, failure("MASSDB_LICENSE_CLOCK_RECOVERY_CHALLENGE_ACTIVE", "active challenge冻结License写入")
	}
	switch intent {
	case IntentNormal:
		if s.ActiveLicense != nil && candidate.ExpiresAt <= s.ActiveLicense.ExpiresAt {
			return State{}, failure("MASSDB_LICENSE_EXPIRY_NOT_EXTENDED", "NORMAL续期必须严格延长")
		}
		for _, barrier := range s.LicenseCorrectionBarriers {
			if candidate.ExpiresAt >= barrier.SupersededExpiresAt &&
				candidate.IssuedAt <= barrier.SupersededIssuedAtCutoff {
				return State{}, failure("MASSDB_LICENSE_SUPERSEDED", "候选License会撤销已批准的到期更正")
			}
		}
	case IntentReplaceWithShorter:
		if s.ActiveLicense == nil || candidate.ExpiresAt >= s.ActiveLicense.ExpiresAt {
			return State{}, failure("MASSDB_LICENSE_CORRECTION_REQUIRED", "到期更正必须严格缩短")
		}
		if requester == "" || approver == "" || requester == approver {
			return State{}, failure("MASSDB_LICENSE_CORRECTION_DUAL_CONTROL_REQUIRED", "到期更正需要异人审批")
		}
	case IntentKeyRotationReplacement:
		if s.ActiveLicense == nil || candidate.ExpiresAt != s.ActiveLicense.ExpiresAt ||
			candidate.SHA256 == s.ActiveLicense.SHA256 {
			return State{}, failure("MASSDB_LICENSE_KEY_ROTATION_REPLACEMENT_INVALID", "key换签替换语义错误")
		}
	default:
		return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "未知License import intent")
	}
	next := s.Clone()
	next.Mutation = &Mutation{OperationID: operationID, Kind: MutationLicense, Intent: intent,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash, Requester: requester,
		Approver: approver, PreparedAt: now, DeadlineAt: deadlineAt,
		CandidateLicense: cloneLicense(&candidate), State: OperationPrepared}
	next.Idempotency[idempotencyKey] = IdempotencyRecord{RequestHash: requestHash,
		OperationID: operationID, State: OperationPrepared}
	return next, nil
}

func (s State) PrepareKeyset(operationID, idempotencyKey, requestHash string,
	kind MutationKind, candidate ActiveKeyset, now, deadlineAt uint64) (State, error) {
	if err := s.requireInitialized(); err != nil {
		return State{}, err
	}
	if err := validateOperation(operationID, idempotencyKey, requestHash, now, deadlineAt); err != nil {
		return State{}, err
	}
	if replay, found, err := s.replayIdempotency(idempotencyKey, requestHash); found || err != nil {
		return replay, err
	}
	if s.Mutation != nil {
		return State{}, failure("MASSDB_LICENSE_MUTATION_IN_PROGRESS", "已有License mutation占用统一槽位")
	}
	if kind != MutationAdditiveKeyset && kind != MutationRestrictiveKeyset {
		return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "非法keyset mutation类型")
	}
	if err := s.validateCandidateKeyset(candidate); err != nil {
		return State{}, err
	}
	if s.ClockChallenge.active(now) && kind == MutationAdditiveKeyset {
		return State{}, failure("MASSDB_LICENSE_CLOCK_RECOVERY_CHALLENGE_ACTIVE", "active challenge冻结加法keyset")
	}
	next := s.Clone()
	next.Mutation = &Mutation{OperationID: operationID, Kind: kind,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash, PreparedAt: now,
		DeadlineAt: deadlineAt, CandidateKeyset: cloneActiveKeyset(&candidate), State: OperationPrepared}
	next.Idempotency[idempotencyKey] = IdempotencyRecord{RequestHash: requestHash,
		OperationID: operationID, State: OperationPrepared}
	return next, nil
}

func (s State) PrepareRecoveryBundle(operationID, idempotencyKey, requestHash string,
	candidateKeyset ActiveKeyset, candidateLicense ActiveLicense,
	now, deadlineAt uint64) (State, error) {
	if err := s.requireInitialized(); err != nil {
		return State{}, err
	}
	if err := validateOperation(operationID, idempotencyKey, requestHash, now, deadlineAt); err != nil {
		return State{}, err
	}
	if replay, found, err := s.replayIdempotency(idempotencyKey, requestHash); found || err != nil {
		return replay, err
	}
	if s.Mutation != nil {
		return State{}, failure("MASSDB_LICENSE_MUTATION_IN_PROGRESS", "已有License mutation占用统一槽位")
	}
	if err := s.validateCandidateKeyset(candidateKeyset); err != nil {
		return State{}, err
	}
	if err := validateLicense(candidateLicense); err != nil {
		return State{}, err
	}
	next := s.Clone()
	next.Mutation = &Mutation{OperationID: operationID,
		Kind: MutationKeysetLicenseRecoveryBundle, IdempotencyKey: idempotencyKey,
		RequestHash: requestHash, PreparedAt: now, DeadlineAt: deadlineAt,
		CandidateKeyset:  cloneActiveKeyset(&candidateKeyset),
		CandidateLicense: cloneLicense(&candidateLicense), State: OperationPrepared}
	next.Idempotency[idempotencyKey] = IdempotencyRecord{RequestHash: requestHash,
		OperationID: operationID, State: OperationPrepared}
	return next, nil
}

func (s State) Commit(operationID string, now uint64) (State, error) {
	if s.Mutation == nil || s.Mutation.OperationID != operationID || s.Mutation.State != OperationPrepared {
		return State{}, failure("MASSDB_LICENSE_OPERATION_NOT_FOUND", "没有匹配的prepared mutation")
	}
	if now > s.Mutation.DeadlineAt {
		return State{}, failure("MASSDB_LICENSE_OPERATION_DEADLINE_EXCEEDED", "prepared mutation已经超时")
	}
	next := s.Clone()
	committed := next.Mutation
	if committed.Kind == MutationLicense {
		if committed.Intent == IntentReplaceWithShorter {
			cutoff := saturatedAdd(now, IssuedAtFutureToleranceSeconds)
			next.LicenseCorrectionBarriers = append(next.LicenseCorrectionBarriers, CorrectionBarrier{
				SupersededExpiresAt: next.ActiveLicense.ExpiresAt, SupersededIssuedAtCutoff: cutoff,
				CorrectedAt: now, SupersededLicenseID: next.ActiveLicense.LicenseID,
				SupersededSHA256: next.ActiveLicense.SHA256})
		}
		next.ActiveLicense = cloneLicense(committed.CandidateLicense)
	} else if committed.Kind == MutationAdditiveKeyset || committed.Kind == MutationRestrictiveKeyset ||
		committed.Kind == MutationKeysetLicenseRecoveryBundle {
		if committed.CandidateKeyset == nil {
			return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "prepared mutation缺少keyset工件")
		}
		next.ActiveKeyset = cloneActiveKeyset(committed.CandidateKeyset)
		if committed.Kind == MutationKeysetLicenseRecoveryBundle {
			if committed.CandidateLicense == nil {
				return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "恢复bundle缺少License工件")
			}
			next.ActiveLicense = cloneLicense(committed.CandidateLicense)
		}
		if (committed.Kind == MutationRestrictiveKeyset ||
			committed.Kind == MutationKeysetLicenseRecoveryBundle) && next.ClockChallenge.active(now) {
			next.ClockChallenge.State = ChallengeInvalidatedByKeysetRecovery
			invalidatedAt := now
			next.ClockChallenge.InvalidatedAt = &invalidatedAt
		}
	}
	committed.State = OperationSucceeded
	record := next.Idempotency[committed.IdempotencyKey]
	record.State = OperationSucceeded
	next.Idempotency[committed.IdempotencyKey] = record
	next.LastOperation = committed
	next.Mutation = nil
	return next, nil
}

func (s State) Abort(operationID string) (State, error) {
	if s.Mutation == nil || s.Mutation.OperationID != operationID {
		return State{}, failure("MASSDB_LICENSE_OPERATION_NOT_FOUND", "没有匹配的active mutation")
	}
	next := s.Clone()
	next.Mutation.State = OperationAborted
	record := next.Idempotency[next.Mutation.IdempotencyKey]
	record.State = OperationAborted
	next.Idempotency[next.Mutation.IdempotencyKey] = record
	next.LastOperation = next.Mutation
	next.Mutation = nil
	return next, nil
}

func (s State) CreateClockChallenge(challengeID, challengeHex string,
	now, expiresAt uint64) (State, error) {
	if err := s.requireInitialized(); err != nil {
		return State{}, err
	}
	if s.Mutation != nil {
		return State{}, failure("MASSDB_LICENSE_MUTATION_IN_PROGRESS", "已有License mutation占用统一槽位")
	}
	if s.ClockChallenge.active(now) {
		return State{}, failure("MASSDB_LICENSE_CLOCK_RECOVERY_CHALLENGE_ACTIVE", "已有active challenge")
	}
	if challengeID == "" || challengeHex == "" || expiresAt <= now {
		return State{}, failure("MASSDB_LICENSE_FILE_INVALID", "challenge字段错误")
	}
	next := s.Clone()
	next.ClockChallenge = &ClockChallenge{ChallengeID: challengeID, ChallengeHex: challengeHex,
		CreatedAt: now, ExpiresAt: expiresAt, State: ChallengeActive}
	return next, nil
}

func (s State) requireInitialized() error {
	if s.EnforcementMode == ModeUninitialized || s.LicenseControlDeploymentUUID == "" {
		return failure("MASSDB_LICENSE_BOOTSTRAP_REQUIRED", "License一致性状态尚未bootstrap")
	}
	return nil
}

func (s State) replayIdempotency(key, requestHash string) (State, bool, error) {
	record, found := s.Idempotency[key]
	if !found {
		return State{}, false, nil
	}
	if record.RequestHash != requestHash {
		return State{}, true, failure("MASSDB_LICENSE_IDEMPOTENCY_CONFLICT", "Idempotency-Key已绑定不同请求")
	}
	return s.Clone(), true, nil
}

func validateOperation(operationID, key, requestHash string, now, deadlineAt uint64) error {
	if operationID == "" || !sha256Pattern.MatchString(requestHash) || deadlineAt <= now {
		return failure("MASSDB_LICENSE_FILE_INVALID", "operation字段错误")
	}
	if len(key) == 0 || len(key) > MaxIdempotencyKeyBytes {
		return failure("MASSDB_LICENSE_IDEMPOTENCY_KEY_INVALID", "Idempotency-Key必须为1至191 ASCII字节")
	}
	for _, b := range []byte(key) {
		if b < 0x21 || b > 0x7e {
			return failure("MASSDB_LICENSE_IDEMPOTENCY_KEY_INVALID", "Idempotency-Key含非法ASCII字符")
		}
	}
	return nil
}

func validateLicense(candidate ActiveLicense) error {
	if candidate.LicenseID == "" || candidate.Kid == "" || !sha256Pattern.MatchString(candidate.SHA256) ||
		candidate.ExpiresAt <= candidate.IssuedAt || len(candidate.Artifact) == 0 {
		return failure("MASSDB_LICENSE_FILE_INVALID", "候选License字段错误")
	}
	return nil
}

func (s State) validateCandidateKeyset(candidate ActiveKeyset) error {
	currentVersion := uint64(0)
	if s.ActiveKeyset != nil {
		currentVersion = s.ActiveKeyset.Version
	}
	if candidate.Version <= currentVersion {
		return failure("MASSDB_LICENSE_KEYSET_VERSION_NOT_INCREASED", "keysetVersion必须严格递增")
	}
	if !sha256Pattern.MatchString(candidate.SHA256) || len(candidate.Artifact) == 0 {
		return failure("MASSDB_LICENSE_FILE_INVALID", "候选keyset字段错误")
	}
	return nil
}

func cloneLicense(value *ActiveLicense) *ActiveLicense {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Artifact = append([]byte(nil), value.Artifact...)
	return &cloned
}

func cloneActiveKeyset(value *ActiveKeyset) *ActiveKeyset {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Artifact = append([]byte(nil), value.Artifact...)
	return &cloned
}

func saturatedAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

// Store provides a race-free in-memory view; Raft is responsible for durable ordering.
type Store struct {
	mu    sync.RWMutex
	state State
}

func NewStore() *Store { return &Store{state: EmptyState()} }

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Clone()
}

func (s *Store) Restore(state State) error {
	if state.FormatVersion != int(FormatVersion) {
		return fmt.Errorf("unsupported MassDB License state format %d", state.FormatVersion)
	}
	state.normalize()
	s.mu.Lock()
	s.state = state.Clone()
	s.mu.Unlock()
	return nil
}
