package kubernetes

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"testing"
)

func TestDiagnoseErrorReturnsSafeActionableClasses(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{name: "invalid arguments", err: ErrInvalidArguments, code: "invalid_arguments"},
		{name: "invalid endpoint", err: ErrInvalidEndpoint, code: "connector_configuration_invalid"},
		{name: "credentials", err: ErrCredentialUnavailable, code: "credential_unavailable"},
		{name: "authentication", err: ErrUnauthorized, code: "cluster_authentication_failed"},
		{name: "permission", err: ErrForbidden, code: "cluster_permission_denied"},
		{name: "missing resource", err: ErrNotFound, code: "resource_unavailable"},
		{name: "timeout", err: context.DeadlineExceeded, code: "request_timeout", retryable: true},
		{name: "budget", err: ErrOperationBudget, code: "operation_budget_exhausted", retryable: true},
		{name: "incomplete graph", err: ErrGraphIncomplete, code: "graph_incomplete", retryable: true},
		{name: "graph limit", err: ErrCompleteGraphLimit, code: "graph_limit_exceeded", retryable: false},
		{name: "response cap", err: ErrResponseTooLarge, code: "response_too_large"},
		{name: "redirect", err: ErrRedirect, code: "redirect_refused"},
		{name: "certificate", err: x509.UnknownAuthorityError{}, code: "tls_verification_failed"},
		{name: "dns", err: &net.DNSError{Err: "not found", Name: "redacted.invalid"}, code: "dns_resolution_failed", retryable: true},
		{name: "connection", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private detail")}, code: "connection_failed", retryable: true},
		{name: "fallback", err: errors.New("secret provider response"), code: "read_failed", retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := DiagnoseError(test.err)
			if detail.Code != test.code || detail.Message == "" || detail.Action == "" || detail.Retryable != test.retryable {
				t.Fatalf("detail = %+v", detail)
			}
			if detail.Message == test.err.Error() || detail.Action == test.err.Error() {
				t.Fatalf("raw cause crossed diagnostic boundary: %+v", detail)
			}
		})
	}
}
