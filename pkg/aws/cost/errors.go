package cost

import (
	"context"
	"errors"
	"net"
	"strings"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// The closed error vocabulary. Provider messages are never echoed; a
// message is read only to tell Cost Explorer being off from a denial.
const (
	// CodeDenied: the role lacks the read permission.
	CodeDenied = "ce_denied"
	// CodeUnavailable: Cost Explorer is not enabled in the account, or the
	// management account turned member access off. AWS does not tell these
	// apart reliably, so the page lists both.
	CodeUnavailable = "ce_unavailable"
	// CodeDataNotReady: Cost Explorer was opened less than 24 hours ago.
	CodeDataNotReady = "ce_data_not_ready"
	// CodeThrottled: AWS throttled the request; nothing is retried.
	CodeThrottled = "throttled"
	// CodeTimeout: a deadline, timeout or transport failure.
	CodeTimeout = "timeout"
	// CodeCredentials: the credentials the request was signed with were
	// rejected or could not be loaded (expired, revoked, missing). The data
	// stays readable but stale until access is back.
	CodeCredentials = "credentials_invalid"
	// CodeBusy: another replica was doing cost work (the paid-request meter
	// kept changing under it), so the call was not sent; a later run sends
	// it. It is the API's code for ErrBusy.
	CodeBusy = "refresh_in_progress"
)

// ErrCostLinesBudgetExceeded reports a period holding more stored cost
// lines than one read may load. It is a refusal, never a partial answer:
// a truncated line set would sum to a wrong total, so the caller narrows the
// period instead. Totals are not bounded by the line budget because the
// store returns a few hundred groups, not the lines behind them.
var ErrCostLinesBudgetExceeded = errors.New("cost: the period holds more cost lines than this deployment can summarise at once; narrow the period")

// Error is a failure reported only by its closed code.
type Error struct{ Code string }

func (e *Error) Error() string { return "cost: " + e.Code }

// Codes lists the vocabulary, for contract tests.
var Codes = []string{CodeDenied, CodeUnavailable, CodeDataNotReady, CodeThrottled, CodeTimeout, CodeCredentials}

// ErrorCode maps a call failure onto the closed vocabulary. A paid-request
// cap refusal is not a provider error and returns "".
func ErrorCode(err error) string {
	if err == nil || errors.Is(err, finops.ErrCapReached) {
		return ""
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	if errors.Is(err, errMalformed) {
		return CodeUnavailable
	}
	if errors.Is(err, ErrBusy) {
		return CodeBusy
	}
	if credentialsRejected(err) {
		return CodeCredentials
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "AccessDeniedException", "AccessDenied", "UnauthorizedOperation",
			"UnauthorizedException", "AuthorizationError", "Forbidden":
			if notEnabled(apiError.ErrorMessage()) {
				return CodeUnavailable
			}
			return CodeDenied
		case "DataUnavailableException":
			return CodeDataNotReady
		case "LimitExceededException", "ThrottlingException", "Throttling",
			"TooManyRequestsException", "RequestLimitExceeded":
			return CodeThrottled
		case "RequestTimeout", "RequestTimeoutException":
			return CodeTimeout
		}
		return CodeUnavailable
	}
	if errors.Is(err, limit.ErrBackoff) || errors.Is(err, awsauth.ErrThrottled) {
		return CodeThrottled
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CodeTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return CodeTimeout
	}
	return CodeTimeout
}

// credentialsRejected reports a failure of the request's credentials rather
// than of the account's permissions: AWS rejected the token or key (expired,
// revoked, malformed), or the signer could not load credentials at all.
// Cost Explorer is a global endpoint, so an unrecognised token here is never
// a disabled region.
func credentialsRejected(err error) bool {
	var signing *v4.SigningError
	if errors.As(err, &signing) || errors.Is(err, awsauth.ErrNoServerIdentity) {
		return true
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) || notEnabled(apiError.ErrorMessage()) {
		return false
	}
	switch apiError.ErrorCode() {
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException",
		"InvalidAccessKeyId", "SignatureDoesNotMatch", "MissingAuthenticationToken",
		"MissingAuthenticationTokenException", "AuthFailure":
		return true
	}
	return false
}

// notEnabled recognises the wording AWS uses when Cost Explorer has not been
// opened or member access is off. The text is matched, never forwarded.
func notEnabled(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "not enabled for cost explorer") ||
		strings.Contains(m, "cost explorer is not enabled") ||
		strings.Contains(m, "linked account doesn't have access")
}
