package massdblicense

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	FormatVersion                  = uint64(1)
	Product                        = "MASSDB"
	KeysetType                     = "MASSDB_TRUSTED_KEYSET"
	ClockRecoveryType              = "MASSDB_CLOCK_RECOVERY"
	RecoveryBundleType             = "MASSDB_KEYSET_LICENSE_RECOVERY_BUNDLE"
	IssuedAtFutureToleranceSeconds = uint64(300)
	MaxClockRecoveryTermSeconds    = uint64(259200)
	maxArtifactBytes               = 65536
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func failure(code, message string) error { return &Error{Code: code, Message: message} }

func Code(err error) string {
	if err == nil {
		return "VALID"
	}
	if protocolError, ok := err.(*Error); ok {
		return protocolError.Code
	}
	return "MASSDB_LICENSE_FILE_INVALID"
}

type KeyUse string

const (
	LicenseSigning KeyUse = "LICENSE_SIGNING"
	ClockRecovery  KeyUse = "CLOCK_RECOVERY"
)

type License struct {
	FormatVersion uint64
	LicenseID     string
	Product       string
	IssuedAt      uint64
	ExpiresAt     uint64
}

type TrustedKey struct {
	Kid       string
	Use       KeyUse
	PublicKey *ecdsa.PublicKey
}

type TrustedKeyset struct {
	FormatVersion uint64
	ArtifactType  string
	Product       string
	Version       uint64
	IssuedAt      uint64
	Keys          map[string]TrustedKey
	RevokedKids   map[string]struct{}
}

type VerifiedKeyset struct {
	Payload TrustedKeyset
	RootKid string
	SHA256  string

	trustedKeys map[string]TrustedKey
	revokedKids map[string]struct{}
}

type VerifiedLicense struct {
	Payload License
	Kid     string
	SHA256  string
}

type ClockRecoveryPayload struct {
	FormatVersion            uint64
	ArtifactType             string
	Product                  string
	RecoverySequence         uint64
	Challenge                []byte
	DeploymentUUID           string
	ActiveLicenseSHA256      []byte
	ObservedMaxSeenWallClock uint64
	ResetMaxSeenWallClockTo  uint64
	IssuedAt                 uint64
	ArtifactExpiresAt        uint64
}

type ClockContext struct {
	Challenge                []byte
	DeploymentUUID           string
	ActiveLicenseSHA256      []byte
	ObservedMaxSeenWallClock uint64
	MaxAcceptedSequence      uint64
	CurrentWallClock         uint64
}

type VerifiedClockRecovery struct {
	Payload ClockRecoveryPayload
	Kid     string
	SHA256  string
}

func cloneKeyset(value TrustedKeyset) TrustedKeyset {
	value.Keys = cloneTrustedKeys(value.Keys)
	value.RevokedKids = cloneStringSet(value.RevokedKids)
	return value
}

func cloneTrustedKeys(source map[string]TrustedKey) map[string]TrustedKey {
	result := make(map[string]TrustedKey, len(source))
	for kid, key := range source {
		if key.PublicKey == nil || key.PublicKey.X == nil || key.PublicKey.Y == nil {
			continue
		}
		key.PublicKey = &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).Set(key.PublicKey.X),
			Y:     new(big.Int).Set(key.PublicKey.Y),
		}
		result[kid] = key
	}
	return result
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for value := range source {
		result[value] = struct{}{}
	}
	return result
}

func ReadP256PublicKey(path string) (*ecdsa.PublicKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "无法读取root公钥")
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(encoded) == 0 || len(encoded) > 16384 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "root公钥PEM为空或超过16384字节")
	}
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "root公钥必须是单个PUBLIC KEY PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "root公钥无法解析")
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "root公钥必须是P-256")
	}
	return key, nil
}

func VerifyKeyset(artifact []byte, roots map[string]*ecdsa.PublicKey, effectiveNow uint64, currentVersion *uint64) (*VerifiedKeyset, error) {
	message, err := parseCose(artifact)
	if err != nil {
		return nil, err
	}
	root := roots[message.kid]
	if root == nil {
		return nil, failure("MASSDB_LICENSE_SIGNATURE_INVALID", "未知keyset root kid")
	}
	if err := verifyCose(message, root); err != nil {
		return nil, err
	}
	payload, err := decodeKeyset(message.payload)
	if err != nil {
		return nil, err
	}
	for _, trustedKey := range payload.Keys {
		if root.X.Cmp(trustedKey.PublicKey.X) == 0 && root.Y.Cmp(trustedKey.PublicKey.Y) == 0 {
			return nil, failure("MASSDB_LICENSE_FILE_INVALID", "keyset离线root不能复用为License或时钟签发key")
		}
	}
	if payload.IssuedAt > addSaturated(effectiveNow, IssuedAtFutureToleranceSeconds) {
		return nil, failure("MASSDB_LICENSE_ISSUED_AT_IN_FUTURE", "keyset issuedAt超过未来容差")
	}
	if currentVersion != nil && payload.Version <= *currentVersion {
		return nil, failure("MASSDB_LICENSE_KEYSET_VERSION_NOT_INCREASED", "keysetVersion必须严格递增")
	}
	return &VerifiedKeyset{
		Payload:     cloneKeyset(payload),
		RootKid:     message.kid,
		SHA256:      digestHex(artifact),
		trustedKeys: cloneTrustedKeys(payload.Keys),
		revokedKids: cloneStringSet(payload.RevokedKids),
	}, nil
}

func VerifyLicense(artifact []byte, keyset *VerifiedKeyset, effectiveNow, maxTerm uint64, currentExpiresAt *uint64) (*VerifiedLicense, error) {
	if keyset == nil || keyset.trustedKeys == nil || keyset.revokedKids == nil || maxTerm == 0 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "必须提供keyset和显式最大期限")
	}
	message, err := parseCose(artifact)
	if err != nil {
		return nil, err
	}
	key, ok := keyset.trustedKeys[message.kid]
	if !ok || key.Use != LicenseSigning {
		return nil, failure("MASSDB_LICENSE_SIGNATURE_INVALID", "kid不存在或用途不匹配")
	}
	if _, revoked := keyset.revokedKids[message.kid]; revoked {
		return nil, failure("MASSDB_LICENSE_SIGNATURE_INVALID", "kid已被吊销")
	}
	if err := verifyCose(message, key.PublicKey); err != nil {
		return nil, err
	}
	payload, err := decodeLicense(message.payload)
	if err != nil {
		return nil, err
	}
	if payload.IssuedAt > addSaturated(effectiveNow, IssuedAtFutureToleranceSeconds) {
		return nil, failure("MASSDB_LICENSE_ISSUED_AT_IN_FUTURE", "issuedAt超过未来容差")
	}
	if payload.ExpiresAt-payload.IssuedAt > maxTerm {
		return nil, failure("MASSDB_LICENSE_TERM_EXCEEDED", "License期限超过目标上限")
	}
	if effectiveNow >= payload.ExpiresAt {
		return nil, failure("MASSDB_LICENSE_EXPIRED", "License已到期")
	}
	if currentExpiresAt != nil && payload.ExpiresAt <= *currentExpiresAt {
		return nil, failure("MASSDB_LICENSE_EXPIRY_NOT_EXTENDED", "NORMAL续期未严格延长")
	}
	return &VerifiedLicense{Payload: payload, Kid: message.kid, SHA256: digestHex(artifact)}, nil
}

func VerifyClockRecovery(artifact []byte, keyset *VerifiedKeyset, context ClockContext) (*VerifiedClockRecovery, error) {
	if keyset == nil || keyset.trustedKeys == nil || keyset.revokedKids == nil {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "必须提供keyset")
	}
	message, err := parseCose(artifact)
	if err != nil {
		return nil, err
	}
	key, ok := keyset.trustedKeys[message.kid]
	if !ok || key.Use != ClockRecovery {
		return nil, failure("MASSDB_LICENSE_SIGNATURE_INVALID", "clock kid不存在或用途不匹配")
	}
	if _, revoked := keyset.revokedKids[message.kid]; revoked {
		return nil, failure("MASSDB_LICENSE_SIGNATURE_INVALID", "clock kid已被吊销")
	}
	if err := verifyCose(message, key.PublicKey); err != nil {
		return nil, err
	}
	payload, err := decodeClock(message.payload)
	if err != nil {
		return nil, err
	}
	if context.CurrentWallClock < payload.IssuedAt || context.CurrentWallClock > payload.ArtifactExpiresAt ||
		payload.ArtifactExpiresAt-payload.IssuedAt > MaxClockRecoveryTermSeconds {
		return nil, failure("MASSDB_LICENSE_EXPIRED", "clock recovery工件不在有效窗口")
	}
	if payload.RecoverySequence <= context.MaxAcceptedSequence {
		return nil, failure("MASSDB_LICENSE_CLOCK_RECOVERY_REPLAY", "recoverySequence已使用")
	}
	if !bytes.Equal(payload.Challenge, context.Challenge) || payload.DeploymentUUID != context.DeploymentUUID ||
		!bytes.Equal(payload.ActiveLicenseSHA256, context.ActiveLicenseSHA256) ||
		payload.ObservedMaxSeenWallClock != context.ObservedMaxSeenWallClock {
		return nil, failure("MASSDB_LICENSE_CLOCK_RECOVERY_CONTEXT_MISMATCH", "clock recovery上下文不匹配")
	}
	return &VerifiedClockRecovery{Payload: payload, Kid: message.kid, SHA256: digestHex(artifact)}, nil
}

func VerifyRecoveryBundle(bundle []byte, roots map[string]*ecdsa.PublicKey, effectiveNow, maxTerm, currentVersion uint64) (*VerifiedLicense, error) {
	value, err := decodeAll(bundle)
	if err != nil {
		return nil, err
	}
	fields, err := exactMap(value, 1, 2, 3, 4)
	if err != nil || fields[1].unsigned != FormatVersion || fields[2].text != RecoveryBundleType || fields[3].kind != kindBytes || fields[4].kind != kindBytes {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "recovery bundle字段错误")
	}
	keyset, err := VerifyKeyset(fields[3].bytes, roots, effectiveNow, &currentVersion)
	if err != nil {
		return nil, err
	}
	return VerifyLicense(fields[4].bytes, keyset, effectiveNow, maxTerm, nil)
}

type coseMessage struct {
	kid       string
	protected []byte
	payload   []byte
	signature []byte
}

func parseCose(encoded []byte) (*coseMessage, error) {
	if len(encoded) == 0 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "工件不能为空")
	}
	if len(encoded) > maxArtifactBytes {
		return nil, failure("MASSDB_LICENSE_FILE_TOO_LARGE", "工件超过65536字节")
	}
	root, err := decodeAll(encoded)
	if err != nil {
		return nil, err
	}
	if root.kind != kindTag || root.unsigned != 18 || root.child == nil || root.child.kind != kindArray || len(root.child.array) != 4 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "工件不是tag 18 COSE_Sign1")
	}
	items := root.child.array
	if items[0].kind != kindBytes || items[1].kind != kindMap || len(items[1].object) != 0 ||
		items[2].kind != kindBytes || items[3].kind != kindBytes || len(items[3].bytes) != 64 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "COSE_Sign1字段错误")
	}
	protected, err := decodeAll(items[0].bytes)
	if err != nil {
		return nil, err
	}
	fields, err := exactMap(protected, 1, 4)
	if err != nil || fields[1].kind != kindNegative || fields[1].negative != -7 || fields[4].kind != kindBytes {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "protected header必须包含alg=-7和kid")
	}
	kid := string(fields[4].bytes)
	if !printableASCII(kid) || len(kid) > 128 {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "kid必须是可打印ASCII")
	}
	return &coseMessage{kid: kid, protected: items[0].bytes, payload: items[2].bytes, signature: items[3].bytes}, nil
}

func verifyCose(message *coseMessage, key *ecdsa.PublicKey) error {
	if key == nil || key.Curve != elliptic.P256() || !key.Curve.IsOnCurve(key.X, key.Y) {
		return failure("MASSDB_LICENSE_SIGNATURE_INVALID", "公钥不是有效P-256")
	}
	toBeSigned := encode(value{kind: kindArray, array: []value{
		{kind: kindText, text: "Signature1"},
		{kind: kindBytes, bytes: message.protected},
		{kind: kindBytes, bytes: []byte{}},
		{kind: kindBytes, bytes: message.payload},
	}})
	digest := sha256.Sum256(toBeSigned)
	r := new(big.Int).SetBytes(message.signature[:32])
	s := new(big.Int).SetBytes(message.signature[32:])
	if r.Sign() <= 0 || s.Sign() <= 0 || !ecdsa.Verify(key, digest[:], r, s) {
		return failure("MASSDB_LICENSE_SIGNATURE_INVALID", "ES256签名验证失败")
	}
	return nil
}

func decodeLicense(payload []byte) (License, error) {
	value, err := decodeAll(payload)
	if err != nil {
		return License{}, err
	}
	fields, err := exactMap(value, 1, 2, 3, 4, 5)
	if err != nil {
		return License{}, err
	}
	if fields[1].kind != kindUnsigned || fields[2].kind != kindText || fields[3].kind != kindText ||
		fields[4].kind != kindUnsigned || fields[5].kind != kindUnsigned {
		return License{}, failure("MASSDB_LICENSE_FILE_INVALID", "License字段类型错误")
	}
	license := License{fields[1].unsigned, fields[2].text, fields[3].text, fields[4].unsigned, fields[5].unsigned}
	if license.FormatVersion != FormatVersion || !canonicalUUID(license.LicenseID) {
		return License{}, failure("MASSDB_LICENSE_FILE_INVALID", "License版本或licenseId错误")
	}
	if license.Product != Product {
		return License{}, failure("MASSDB_LICENSE_PRODUCT_MISMATCH", "License product必须为MASSDB")
	}
	if license.IssuedAt >= license.ExpiresAt {
		return License{}, failure("MASSDB_LICENSE_FILE_INVALID", "必须满足issuedAt < expiresAt")
	}
	return license, nil
}

func decodeKeyset(payload []byte) (TrustedKeyset, error) {
	value, err := decodeAll(payload)
	if err != nil {
		return TrustedKeyset{}, err
	}
	fields, err := exactMap(value, 1, 2, 3, 4, 5, 6, 7)
	if err != nil {
		return TrustedKeyset{}, err
	}
	if fields[1].kind != kindUnsigned || fields[2].kind != kindText || fields[3].kind != kindText ||
		fields[4].kind != kindUnsigned || fields[5].kind != kindUnsigned || fields[6].kind != kindArray || fields[7].kind != kindArray ||
		fields[1].unsigned != FormatVersion || fields[2].text != KeysetType || fields[3].text != Product || fields[4].unsigned == 0 || len(fields[6].array) == 0 {
		return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "keyset固定字段错误")
	}
	result := TrustedKeyset{FormatVersion: fields[1].unsigned, ArtifactType: fields[2].text, Product: fields[3].text,
		Version: fields[4].unsigned, IssuedAt: fields[5].unsigned, Keys: map[string]TrustedKey{}, RevokedKids: map[string]struct{}{}}
	lastKid := ""
	coordinates := map[string]struct{}{}
	for _, row := range fields[6].array {
		keyFields, keyErr := exactMap(row, 1, 2, 3, 4)
		if keyErr != nil || keyFields[1].kind != kindText || keyFields[2].kind != kindText ||
			keyFields[3].kind != kindBytes || len(keyFields[3].bytes) != 32 || keyFields[4].kind != kindBytes || len(keyFields[4].bytes) != 32 {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "trusted key字段错误")
		}
		kid := keyFields[1].text
		use := KeyUse(keyFields[2].text)
		if !printableASCII(kid) || kid <= lastKid || (use != LicenseSigning && use != ClockRecovery) {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "trusted key顺序、kid或用途错误")
		}
		x := new(big.Int).SetBytes(keyFields[3].bytes)
		y := new(big.Int).SetBytes(keyFields[4].bytes)
		publicKey := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !publicKey.Curve.IsOnCurve(x, y) {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "trusted key坐标不在P-256曲线")
		}
		coordinateKey := hex.EncodeToString(keyFields[3].bytes) + hex.EncodeToString(keyFields[4].bytes)
		if _, exists := coordinates[coordinateKey]; exists {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "同一P-256公钥不能复用为不同kid或用途")
		}
		coordinates[coordinateKey] = struct{}{}
		result.Keys[kid] = TrustedKey{Kid: kid, Use: use, PublicKey: publicKey}
		lastKid = kid
	}
	lastKid = ""
	for _, row := range fields[7].array {
		if row.kind != kindText || !printableASCII(row.text) || row.text <= lastKid {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "revokedKids顺序或值错误")
		}
		if _, exists := result.Keys[row.text]; exists {
			return TrustedKeyset{}, failure("MASSDB_LICENSE_FILE_INVALID", "revoked kid仍在active keys")
		}
		result.RevokedKids[row.text] = struct{}{}
		lastKid = row.text
	}
	return result, nil
}

func decodeClock(payload []byte) (ClockRecoveryPayload, error) {
	value, err := decodeAll(payload)
	if err != nil {
		return ClockRecoveryPayload{}, err
	}
	fields, err := exactMap(value, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11)
	if err != nil {
		return ClockRecoveryPayload{}, err
	}
	for _, key := range []uint64{1, 4, 8, 9, 10, 11} {
		if fields[key].kind != kindUnsigned {
			return ClockRecoveryPayload{}, failure("MASSDB_LICENSE_FILE_INVALID", "clock uint字段错误")
		}
	}
	if fields[2].kind != kindText || fields[3].kind != kindText || fields[5].kind != kindBytes ||
		fields[6].kind != kindText || fields[7].kind != kindBytes {
		return ClockRecoveryPayload{}, failure("MASSDB_LICENSE_FILE_INVALID", "clock字段类型错误")
	}
	result := ClockRecoveryPayload{
		FormatVersion: fields[1].unsigned, ArtifactType: fields[2].text, Product: fields[3].text,
		RecoverySequence: fields[4].unsigned, Challenge: fields[5].bytes, DeploymentUUID: fields[6].text,
		ActiveLicenseSHA256: fields[7].bytes, ObservedMaxSeenWallClock: fields[8].unsigned,
		ResetMaxSeenWallClockTo: fields[9].unsigned, IssuedAt: fields[10].unsigned, ArtifactExpiresAt: fields[11].unsigned,
	}
	if result.FormatVersion != FormatVersion || result.ArtifactType != ClockRecoveryType || result.Product != Product ||
		result.RecoverySequence == 0 || len(result.Challenge) != 32 || len(result.ActiveLicenseSHA256) != 32 ||
		!canonicalUUID(result.DeploymentUUID) || result.ArtifactExpiresAt <= result.IssuedAt ||
		result.ResetMaxSeenWallClockTo < result.IssuedAt || result.ResetMaxSeenWallClockTo > result.ArtifactExpiresAt ||
		result.ResetMaxSeenWallClockTo >= result.ObservedMaxSeenWallClock {
		return ClockRecoveryPayload{}, failure("MASSDB_LICENSE_FILE_INVALID", "clock固定字段或时间边界错误")
	}
	return result, nil
}

type valueKind uint8

const (
	kindUnsigned valueKind = iota
	kindNegative
	kindBytes
	kindText
	kindArray
	kindMap
	kindTag
)

type value struct {
	kind     valueKind
	unsigned uint64
	negative int64
	bytes    []byte
	text     string
	array    []value
	object   []entry
	child    *value
}

type entry struct {
	key   value
	value value
}

type decoder struct {
	data   []byte
	offset int
}

func decodeAll(encoded []byte) (value, error) {
	if len(encoded) == 0 || len(encoded) > maxArtifactBytes {
		return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR为空或超过限制")
	}
	decoder := &decoder{data: encoded}
	decoded, err := decoder.read(0)
	if err != nil {
		return value{}, err
	}
	if decoder.offset != len(encoded) || !bytes.Equal(encoded, encode(decoded)) {
		return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR存在尾随字节或不是deterministic编码")
	}
	return decoded, nil
}

func (d *decoder) read(depth int) (value, error) {
	if depth > 8 || d.offset >= len(d.data) {
		return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR深度超限或被截断")
	}
	initial := d.data[d.offset]
	d.offset++
	major := initial >> 5
	argument, err := d.argument(initial & 0x1f)
	if err != nil {
		return value{}, err
	}
	switch major {
	case 0:
		return value{kind: kindUnsigned, unsigned: argument}, nil
	case 1:
		if argument > math.MaxInt64 {
			return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR负整数超出实现上限")
		}
		return value{kind: kindNegative, negative: -1 - int64(argument)}, nil
	case 2, 3:
		maximum := uint64(maxArtifactBytes)
		if major == 3 {
			maximum = 4096
		}
		if argument > maximum || argument > uint64(len(d.data)-d.offset) {
			return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR字符串长度错误")
		}
		content := append([]byte(nil), d.data[d.offset:d.offset+int(argument)]...)
		d.offset += int(argument)
		if major == 2 {
			return value{kind: kindBytes, bytes: content}, nil
		}
		if !utf8.Valid(content) {
			return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR文本不是UTF-8")
		}
		return value{kind: kindText, text: string(content)}, nil
	case 4:
		if argument > 128 {
			return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR数组过长")
		}
		items := make([]value, 0, argument)
		for i := uint64(0); i < argument; i++ {
			item, itemErr := d.read(depth + 1)
			if itemErr != nil {
				return value{}, itemErr
			}
			items = append(items, item)
		}
		return value{kind: kindArray, array: items}, nil
	case 5:
		if argument > 128 {
			return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR map过长")
		}
		entries := make([]entry, 0, argument)
		var previous []byte
		for i := uint64(0); i < argument; i++ {
			start := d.offset
			key, keyErr := d.read(depth + 1)
			if keyErr != nil {
				return value{}, keyErr
			}
			encodedKey := append([]byte(nil), d.data[start:d.offset]...)
			if previous != nil && (len(previous) > len(encodedKey) || (len(previous) == len(encodedKey) && bytes.Compare(previous, encodedKey) >= 0)) {
				return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR map key顺序或重复错误")
			}
			item, itemErr := d.read(depth + 1)
			if itemErr != nil {
				return value{}, itemErr
			}
			entries = append(entries, entry{key: key, value: item})
			previous = encodedKey
		}
		return value{kind: kindMap, object: entries}, nil
	case 6:
		child, childErr := d.read(depth + 1)
		if childErr != nil {
			return value{}, childErr
		}
		return value{kind: kindTag, unsigned: argument, child: &child}, nil
	default:
		return value{}, failure("MASSDB_LICENSE_FILE_INVALID", "协议不允许该CBOR major type")
	}
}

func (d *decoder) argument(additional byte) (uint64, error) {
	if additional < 24 {
		return uint64(additional), nil
	}
	count := 0
	switch additional {
	case 24:
		count = 1
	case 25:
		count = 2
	case 26:
		count = 4
	case 27:
		count = 8
	default:
		return 0, failure("MASSDB_LICENSE_FILE_INVALID", "不允许indefinite CBOR")
	}
	if d.offset+count > len(d.data) {
		return 0, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR长度被截断")
	}
	var result uint64
	for i := 0; i < count; i++ {
		result = result<<8 | uint64(d.data[d.offset])
		d.offset++
	}
	minimum := []uint64{0, 24, 256, 0, 65536, 0, 0, 0, 4294967296}[count]
	if result < minimum {
		return 0, failure("MASSDB_LICENSE_FILE_INVALID", "CBOR参数不是最短编码")
	}
	return result, nil
}

func encode(item value) []byte {
	var output bytes.Buffer
	writeValue(&output, item)
	return output.Bytes()
}

func writeValue(output *bytes.Buffer, item value) {
	switch item.kind {
	case kindUnsigned:
		writeHead(output, 0, item.unsigned)
	case kindNegative:
		writeHead(output, 1, uint64(-1-item.negative))
	case kindBytes:
		writeHead(output, 2, uint64(len(item.bytes)))
		output.Write(item.bytes)
	case kindText:
		writeHead(output, 3, uint64(len([]byte(item.text))))
		output.WriteString(item.text)
	case kindArray:
		writeHead(output, 4, uint64(len(item.array)))
		for _, child := range item.array {
			writeValue(output, child)
		}
	case kindMap:
		entries := append([]entry(nil), item.object...)
		sort.Slice(entries, func(i, j int) bool {
			left, right := encode(entries[i].key), encode(entries[j].key)
			if len(left) != len(right) {
				return len(left) < len(right)
			}
			return bytes.Compare(left, right) < 0
		})
		writeHead(output, 5, uint64(len(entries)))
		for _, row := range entries {
			writeValue(output, row.key)
			writeValue(output, row.value)
		}
	case kindTag:
		writeHead(output, 6, item.unsigned)
		writeValue(output, *item.child)
	}
}

func writeHead(output *bytes.Buffer, major byte, argument uint64) {
	switch {
	case argument < 24:
		output.WriteByte(major<<5 | byte(argument))
	case argument <= math.MaxUint8:
		output.WriteByte(major<<5 | 24)
		output.WriteByte(byte(argument))
	case argument <= math.MaxUint16:
		output.WriteByte(major<<5 | 25)
		output.Write([]byte{byte(argument >> 8), byte(argument)})
	case argument <= math.MaxUint32:
		output.WriteByte(major<<5 | 26)
		for shift := 24; shift >= 0; shift -= 8 {
			output.WriteByte(byte(argument >> shift))
		}
	default:
		output.WriteByte(major<<5 | 27)
		for shift := 56; shift >= 0; shift -= 8 {
			output.WriteByte(byte(argument >> shift))
		}
	}
}

func exactMap(item value, keys ...uint64) (map[uint64]value, error) {
	if item.kind != kindMap || len(item.object) != len(keys) {
		return nil, failure("MASSDB_LICENSE_FILE_INVALID", "payload字段数量错误")
	}
	expected := make(map[uint64]struct{}, len(keys))
	for _, key := range keys {
		expected[key] = struct{}{}
	}
	result := make(map[uint64]value, len(keys))
	for _, row := range item.object {
		if row.key.kind != kindUnsigned {
			return nil, failure("MASSDB_LICENSE_FILE_INVALID", "payload key必须是uint")
		}
		if _, ok := expected[row.key.unsigned]; !ok {
			return nil, failure("MASSDB_LICENSE_FILE_INVALID", "payload包含未知key")
		}
		result[row.key.unsigned] = row.value
	}
	return result, nil
}

func printableASCII(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func addSaturated(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func digestHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func ParseHex32(text string) ([]byte, error) {
	decoded, err := hex.DecodeString(text)
	if err != nil || len(decoded) != 32 || text != strings.ToLower(text) {
		return nil, fmt.Errorf("must be 64 lowercase hexadecimal characters")
	}
	return decoded, nil
}
