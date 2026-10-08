package kubernetes

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
)

// ErrorDetail is a bounded operator-facing explanation of a Kubernetes error.
// It never includes provider responses, credentials, endpoints, or raw causes.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Action    string `json:"action"`
	Retryable bool   `json:"retryable"`
}

type connectionError struct {
	cause error
}

func (err *connectionError) Error() string {
	return "kubernetes: connection failed"
}

func (err *connectionError) Unwrap() error {
	return err.cause
}

// DiagnoseError classifies connector failures without exposing their raw cause.
func DiagnoseError(err error) ErrorDetail {
	var dnsError *net.DNSError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var certificateError x509.CertificateInvalidError
	switch {
	case errors.Is(err, ErrLogStreamBusy):
		return ErrorDetail{Code: "log_stream_busy", Message: "The concurrent Pod log stream limit has been reached.", Action: "Close another log stream and retry with backoff.", Retryable: true}
	case errors.Is(err, errPreviousUnavailable):
		return ErrorDetail{Code: "previous_unavailable", Message: "Previous container logs are unavailable.", Action: "Select current container logs.", Retryable: false}
	case errors.Is(err, ErrInvalidArguments):
		return ErrorDetail{Code: "invalid_arguments", Message: "The Kubernetes request is missing or has invalid parameters.", Action: "Check the namespace, resource ID, object name, container, and numeric limits.", Retryable: false}
	case errors.Is(err, ErrInvalidEndpoint):
		return ErrorDetail{Code: "connector_configuration_invalid", Message: "The Kubernetes endpoint or network policy is invalid.", Action: "Check connectors.kubernetes.endpoint, endpoint_cidrs, and allow_private_networks, then restart Versus.", Retryable: false}
	case errors.Is(err, ErrCredentialUnavailable):
		return ErrorDetail{Code: "credential_unavailable", Message: "Kubernetes credentials are unavailable.", Action: "Configure a credential source for connectors.kubernetes.auth.mode and restart Versus. For EKS, provide IRSA, Pod Identity, environment credentials, or a static selected profile.", Retryable: false}
	case errors.Is(err, ErrUnauthorized):
		return ErrorDetail{Code: "cluster_authentication_failed", Message: "The Kubernetes API rejected the connector credentials.", Action: "Verify the cloud identity, EKS/AKS/GKE access mapping, token audience, and credential expiry.", Retryable: false}
	case errors.Is(err, ErrForbidden):
		return ErrorDetail{Code: "cluster_permission_denied", Message: "Kubernetes denied access to this resource.", Action: "Grant the connector identity the required read-only Kubernetes RBAC permissions.", Retryable: false}
	case errors.Is(err, ErrNotFound):
		return ErrorDetail{Code: "resource_unavailable", Message: "The Kubernetes resource or optional API is unavailable.", Action: "Verify the resource ID, namespace, API group, and whether the optional API is installed.", Retryable: false}
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorDetail{Code: "request_timeout", Message: "The Kubernetes read timed out.", Action: "Check API-server reachability and increase connectors.kubernetes.timeout only after resolving network or control-plane latency.", Retryable: true}
	case errors.Is(err, ErrOperationBudget):
		return ErrorDetail{Code: "operation_budget_exhausted", Message: "The Kubernetes read reached its safety budget.", Action: "Narrow the namespace, resource category, search query, or requested result limits.", Retryable: true}
	case errors.Is(err, ErrGraphIncomplete):
		return ErrorDetail{Code: "graph_incomplete", Message: "Kubernetes could not verify every resource in the selected namespace.", Action: "Restore read-only discovery and list access for the namespace's graph resources, then retry.", Retryable: true}
	case errors.Is(err, ErrCompleteGraphLimit):
		return ErrorDetail{Code: "graph_limit_exceeded", Message: "The complete namespace graph exceeds the safe response limit.", Action: "Choose a namespace with fewer resources or reduce its resource count, then retry.", Retryable: false}
	case errors.Is(err, ErrResponseTooLarge):
		return ErrorDetail{Code: "response_too_large", Message: "The Kubernetes API response exceeded the safe size limit.", Action: "Narrow the namespace, selectors, resource category, or result limit.", Retryable: false}
	case errors.Is(err, ErrRedirect):
		return ErrorDetail{Code: "redirect_refused", Message: "The Kubernetes endpoint attempted an HTTP redirect.", Action: "Configure the final HTTPS API-server endpoint directly; redirects are refused for credential safety.", Retryable: false}
	case errors.As(err, &unknownAuthority), errors.As(err, &hostnameError), errors.As(err, &certificateError):
		return ErrorDetail{Code: "tls_verification_failed", Message: "The Kubernetes API certificate could not be verified.", Action: "Verify connectors.kubernetes.ca_data or ca_file and server_name against the API-server certificate.", Retryable: false}
	case errors.As(err, &dnsError):
		return ErrorDetail{Code: "dns_resolution_failed", Message: "The Kubernetes API hostname could not be resolved.", Action: "Verify connectors.kubernetes.endpoint and DNS from the Versus container.", Retryable: true}
	default:
		var networkError *net.OpError
		if errors.As(err, &networkError) {
			return ErrorDetail{Code: "connection_failed", Message: "Versus could not connect to the Kubernetes API.", Action: "Check the endpoint, private-network access, endpoint CIDRs, firewall rules, and security groups from the Versus container.", Retryable: true}
		}
		return ErrorDetail{Code: "read_failed", Message: "The Kubernetes read failed.", Action: "Check the connector configuration and Versus server log for the safe Kubernetes error code.", Retryable: true}
	}
}
