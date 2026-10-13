// Package license verifies MassDB OSS license certificates and decides
// whether data reads are allowed. Certificates are compact Ed25519 JWS
// documents issued offline (protocol v1 shared with MassDB SQL); masters
// import and distribute them, volume servers and filers verify and cache
// them. See docs/license-p0-contract-20261013.md for the frozen contract.
package license

import (
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
)

// Reason is a license status, also used as the deny reason of a read.
type Reason string

const (
	ReasonValid          Reason = "VALID"
	ReasonNoLicense      Reason = "NO_LICENSE"
	ReasonNotYetValid    Reason = "NOT_YET_VALID"
	ReasonExpired        Reason = "EXPIRED"
	ReasonFeatureMissing Reason = "FEATURE_MISSING"
	ReasonClockSuspect   Reason = "CLOCK_SUSPECT"
	// ReasonReconciling is shown by masters only and never sent to nodes.
	ReasonReconciling Reason = "RECONCILING"
	// ReasonNotReady and ReasonNodeRejected exist on volume servers and filers only.
	ReasonNotReady     Reason = "NOT_READY"
	ReasonNodeRejected Reason = "NODE_REJECTED"
)

// Code identifies a management or admission error.
type Code string

const (
	CodeMalformed            Code = "LICENSE_MALFORMED"
	CodeUntrustedKid         Code = "LICENSE_UNTRUSTED_KID"
	CodeSignatureInvalid     Code = "LICENSE_SIGNATURE_INVALID"
	CodeProductMismatch      Code = "LICENSE_PRODUCT_MISMATCH"
	CodeDeploymentMismatch   Code = "LICENSE_DEPLOYMENT_MISMATCH"
	CodeExpired              Code = "LICENSE_EXPIRED"
	CodeRequestTooLarge      Code = "REQUEST_TOO_LARGE"
	CodeSequenceNotIncreased Code = "LICENSE_SEQUENCE_NOT_INCREASING"
	CodeSequenceConflict     Code = "LICENSE_SEQUENCE_CONFLICT"
	CodeNodeLimitTooSmall    Code = "LICENSE_NODE_LIMIT_TOO_SMALL"
	CodeMembershipChanged    Code = "MEMBERSHIP_CHANGED"
	CodeNodeOnline           Code = "NODE_ONLINE"
	CodeNodeNotFound         Code = "NODE_NOT_FOUND"
	CodeRateLimited          Code = "RATE_LIMITED"
	CodeNotLeader            Code = "NOT_LEADER"
	CodeReconciling          Code = "RECONCILING"
	CodePartialCommit        Code = "PARTIAL_COMMIT"
	// CodeNodeLimitExceeded rejects a node registration or master join.
	CodeNodeLimitExceeded Code = "NODE_LIMIT_EXCEEDED"
	// CodeContentHidden rejects writing or emitting an entry whose inline
	// content was hidden from the caller.
	CodeContentHidden Code = "CONTENT_HIDDEN"
)

// HTTPStatus maps a code to the status returned by the master HTTP API.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeMalformed, CodeUntrustedKid, CodeSignatureInvalid, CodeProductMismatch,
		CodeDeploymentMismatch, CodeExpired:
		return http.StatusBadRequest
	case CodeRequestTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeSequenceNotIncreased, CodeSequenceConflict, CodeNodeLimitTooSmall,
		CodeMembershipChanged, CodeNodeOnline, CodeContentHidden:
		return http.StatusConflict
	case CodeNodeNotFound:
		return http.StatusNotFound
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeNodeLimitExceeded:
		return http.StatusForbidden
	default:
		return http.StatusServiceUnavailable
	}
}

// GRPCCode maps a code to the gRPC status code used for the same error.
func (c Code) GRPCCode() codes.Code {
	switch c.HTTPStatus() {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return codes.InvalidArgument
	case http.StatusConflict:
		return codes.FailedPrecondition
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusForbidden:
		return codes.PermissionDenied
	default:
		return codes.Unavailable
	}
}

// Error is a license error with a stable code. Messages never contain
// certificate text.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "license: " + string(e.Code)
	}
	return "license: " + string(e.Code) + " " + e.Message
}

func errorf(code Code, format string, args ...interface{}) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
