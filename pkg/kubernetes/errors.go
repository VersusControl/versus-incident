package kubernetes

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var ErrClusterRequired = errors.New("kubernetes: cluster is required")
var ErrClusterNotFound = errors.New("kubernetes: cluster not found")
var ErrClusterUnavailable = errors.New("kubernetes: cluster unavailable")

// ErrInventoryNotReady reports that a read needs an inventory kind that is not
// yet fresh and complete. It is a retryable state of Versus itself, not a
// permission problem: reporting it as cluster_permission_denied sends an
// operator to change RBAC that was never the cause.
var ErrInventoryNotReady = errors.New("kubernetes: inventory not ready")

// ErrRequestTooLarge reports a request this server refuses to parse because it
// exceeds the surface's own bound. The answer is explicit and bounded, so an
// oversized request can never disappear into an intermediary's header limit.
var ErrRequestTooLarge = errors.New("kubernetes: request too large")

// ErrConnectorNotConfigured reports that this deployment has no Kubernetes
// connector at all. It is one half of a pair: the other half is
// ErrConnectorCredentialUnresolved, and answering one with the other is how a
// surface renders a setup card over a cluster that published data yesterday.
var ErrConnectorNotConfigured = errors.New("kubernetes: connector not configured")

// ErrConnectorCredentialUnresolved reports a connector that is configured but
// whose credential cannot be resolved right now — the cluster is known, its
// published data is still on disk, and only the identity that reads it is gone.
// Its cause stays attached for classification and never renders: the message a
// caller sees names no identity, path, or endpoint.
var ErrConnectorCredentialUnresolved = errors.New("kubernetes: connector credential cannot be resolved")

const (
	// inventoryFreshness is how old the newest listing of a kind may be before
	// a read that depends on it waits for the next cycle.
	inventoryFreshness = 2 * time.Minute
	// inventoryRetryAfter is the bounded backoff hint returned with a not-ready
	// answer. The next inventory cycle is always sooner than this.
	inventoryRetryAfter = 5 * time.Second
	// forbiddenStatusLimit bounds how much of a 403 response body is read.
	forbiddenStatusLimit = 4096
)

// inventoryKindState describes one reason a read has to wait: a kind that has
// never been listed, or a listing that is too old or incomplete.
type inventoryKindState struct {
	Kind     string
	Age      time.Duration
	Listed   bool
	Complete bool
}

type inventoryNotReadyError struct {
	Kinds []inventoryKindState
}

func (err *inventoryNotReadyError) Error() string {
	return "kubernetes: inventory not ready (" + err.Summary() + ")"
}

func (err *inventoryNotReadyError) Unwrap() error { return ErrInventoryNotReady }

// RetryAfter returns the bounded hint for how long to wait before retrying.
func (err *inventoryNotReadyError) RetryAfter() time.Duration { return inventoryRetryAfter }

// Summary names the affected kinds and their age in plain words. It is bounded
// to a few kinds and carries no record names, labels, or values.
func (err *inventoryNotReadyError) Summary() string {
	const shown = 3
	phrases := make([]string, 0, shown)
	for index, kind := range err.Kinds {
		if index == shown {
			phrases = append(phrases, strconv.Itoa(len(err.Kinds)-shown)+" more")
			break
		}
		phrases = append(phrases, inventoryKindPhrase(kind))
	}
	if len(phrases) == 0 {
		return "the inventory is still loading"
	}
	return strings.Join(phrases, "; ")
}

func inventoryKindPhrase(kind inventoryKindState) string {
	name := boundedLogToken(kind.Kind)
	if !kind.Listed {
		return name + " inventory has not been listed yet"
	}
	phrase := name + " inventory is " + inventoryAgeText(kind.Age) + " old"
	if !kind.Complete {
		return phrase + " and incomplete"
	}
	return phrase
}

func inventoryAgeText(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "under a minute"
	case age < time.Hour:
		return strconv.Itoa(int(age/time.Minute)) + " min"
	case age < 48*time.Hour:
		return strconv.Itoa(int(age/time.Hour)) + " h"
	default:
		return strconv.Itoa(int(age/(24*time.Hour))) + " d"
	}
}

// InventoryRetryAfter returns the bounded retry hint for a not-ready answer,
// falling back to the default when the error carries none.
func InventoryRetryAfter(err error) time.Duration {
	var notReady *inventoryNotReadyError
	if errors.As(err, &notReady) {
		if hint := notReady.RetryAfter(); hint > 0 && hint <= time.Minute {
			return hint
		}
	}
	return inventoryRetryAfter
}

// permissionDeniedError reports what a real cluster 403 denied. It carries only
// the verb, resource, and namespace the API server named, plus a classification
// of the denied identity: never the response body, a token, a URL, or the
// identity itself. An IAM ARN contains the cloud account id, so the principal is
// classified rather than quoted.
type permissionDeniedError struct {
	Verb      string
	Resource  string
	Namespace string
	// Principal classifies who was denied, for example "an assumed role". It is
	// derived from the reported identity and never contains it.
	Principal string
}

func (err *permissionDeniedError) Unwrap() error { return ErrForbidden }

func (err *permissionDeniedError) Error() string { return "kubernetes: " + err.describe() }

func (err *permissionDeniedError) describe() string {
	action := "access"
	if err.Verb != "" {
		action = err.Verb
	}
	if err.Resource != "" {
		action += " on " + err.Resource
	}
	if err.Namespace != "" {
		action += " in namespace " + err.Namespace
	} else {
		action += " at cluster scope"
	}
	if err.Principal != "" {
		action += " for " + err.Principal
	}
	return action
}

var (
	deniedUserPattern      = regexp.MustCompile(`User "([^"]{1,253})" cannot`)
	deniedActionPattern    = regexp.MustCompile(`cannot ([a-z]{1,32}) resource "([^"]{1,253})"`)
	deniedNamespacePattern = regexp.MustCompile(`in the namespace "([^"]{1,63})"`)
	// deniedResourceSegment is the only shape a resource or subresource name may
	// have. A slash is handled separately because a read may name a subresource
	// such as pods/log, but two segments is the most that is ever recognised.
	deniedResourceSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	// deniedNamespaceLabel is a DNS-1123 label, the only shape a namespace has.
	deniedNamespaceLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// deniedVerbs is the closed set of verbs Kubernetes reports in a denial. An
// allowlist means no other text can reach a message through the verb match.
var deniedVerbs = map[string]bool{
	"get": true, "list": true, "watch": true, "create": true, "update": true,
	"patch": true, "delete": true, "deletecollection": true,
}

// permissionDeniedFromStatus reads the bounded Kubernetes Status body of a 403
// and keeps only what it can prove: a known verb, a resource-shaped name, a
// namespace-shaped label, and a classification of the denied identity. Every
// other field of the body is dropped, so a hostile or chatty Status response
// cannot carry account ids, tokens, URLs, query strings, addresses, or payload
// text into an error message. A body that is absent, oversized, or shaped
// differently yields no detail and the caller keeps the generic answer.
func permissionDeniedFromStatus(body []byte) error {
	if len(body) == 0 || len(body) > forbiddenStatusLimit {
		return nil
	}
	var status struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		Code       int    `json:"code"`
		Message    string `json:"message"`
		Details    struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &status) != nil || status.Kind != "Status" || status.APIVersion != "v1" || status.Status != "Failure" || status.Code != http.StatusForbidden || status.Reason != "Forbidden" {
		return nil
	}
	denied := &permissionDeniedError{}
	if match := deniedUserPattern.FindStringSubmatch(status.Message); match != nil {
		denied.Principal = deniedPrincipalKind(safeDeniedField(match[1], 253))
	}
	if match := deniedActionPattern.FindStringSubmatch(status.Message); match != nil {
		denied.Verb, denied.Resource = deniedVerb(match[1]), deniedResource(safeDeniedField(match[2], 253))
	}
	if match := deniedNamespacePattern.FindStringSubmatch(status.Message); match != nil {
		denied.Namespace = deniedNamespace(safeDeniedField(match[1], 63))
	}
	if denied.Resource == "" {
		denied.Resource = deniedResource(safeDeniedField(status.Details.Kind, 253))
	}
	if denied.Namespace == "" {
		denied.Namespace = deniedNamespace(safeDeniedField(status.Details.Namespace, 63))
	}
	if denied.Verb == "" && denied.Resource == "" && denied.Namespace == "" && denied.Principal == "" {
		return nil
	}
	return denied
}

// deniedVerb accepts only a verb Kubernetes itself reports for a denial.
func deniedVerb(value string) string {
	if deniedVerbs[value] {
		return value
	}
	return ""
}

// deniedResource accepts one resource name, optionally with one subresource. A
// dotted quad is refused even though it is token-shaped: an address is never a
// resource name, and a denial must never disclose one.
func deniedResource(value string) string {
	if value == "" || looksLikeAddress(value) {
		return ""
	}
	parts := strings.Split(value, "/")
	if len(parts) > 2 {
		return ""
	}
	for _, part := range parts {
		if !deniedResourceSegment.MatchString(part) || looksLikeAddress(part) {
			return ""
		}
	}
	return value
}

// looksLikeAddress reports whether a token is a dotted-quad IPv4 address.
func looksLikeAddress(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 3 {
			return false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || number > 255 {
			return false
		}
	}
	return true
}

// deniedNamespace accepts only a DNS-1123 label.
func deniedNamespace(value string) string {
	if value == "" || !deniedNamespaceLabel.MatchString(value) {
		return ""
	}
	return value
}

// deniedPrincipalKind classifies the denied identity without naming it. The raw
// value is never returned: a cloud ARN embeds the account id, and a local name
// can be a person, so neither leaves this function.
func deniedPrincipalKind(user string) string {
	switch {
	case user == "":
		return ""
	case strings.HasPrefix(user, "system:serviceaccount:"):
		return "a service account"
	case strings.HasPrefix(user, "system:node"):
		return "a node identity"
	case strings.HasPrefix(user, "system:"):
		return "a cluster component"
	case strings.HasPrefix(user, "eks:") || strings.Contains(user, "aws-iam-authenticator"):
		return "an IAM-mapped principal"
	case strings.Contains(user, ":assumed-role/"), strings.Contains(user, ":user/"):
		return "an assumed role"
	case strings.HasPrefix(user, "arn:"):
		return "a cloud principal"
	}
	return "an unknown principal"
}

// safeDeniedField keeps one API-server field bounded and free of anything that
// could steer a terminal or a format string: control runes, DEL, C1 controls,
// quotes, backslashes, and printf verbs are all dropped rather than escaped.
func safeDeniedField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > limit {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '%' || r == '"' || r == '\\' || unicode.IsControl(r) {
			return ""
		}
	}
	return value
}

func DiagnoseClusterError(err error, multiple bool, clusterID string) ErrorDetail {
	detail := DiagnoseError(err)
	if multiple {
		detail.Action = strings.ReplaceAll(detail.Action, "connectors.kubernetes.", "connectors.kubernetes.clusters["+clusterID+"].")
	}
	return detail
}

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
	var throttled *ErrThrottled
	var notReady *inventoryNotReadyError
	var denied *permissionDeniedError
	switch {
	// The two connector states are answered before any credential or network
	// class, because a configured connector whose credential is gone still
	// unwraps to a credential fault and must not be named as one: a surface has
	// to be able to tell "nothing is configured" from "configured, credential
	// gone" to decide between a setup card and an error over the data it has.
	case errors.Is(err, ErrConnectorNotConfigured):
		return ErrorDetail{Code: "connector_not_configured", Message: "Kubernetes connector is not configured", Action: "Configure a Kubernetes connector for this deployment.", Retryable: false}
	case errors.Is(err, ErrConnectorCredentialUnresolved):
		return ErrorDetail{Code: "connector_credential_unresolved", Message: "The Kubernetes connector is configured, but its credential cannot be resolved.", Action: "Restore or rotate the connector credential, then retry. The connector recovers on the next request.", Retryable: true}
	case errors.As(err, &notReady):
		return ErrorDetail{Code: "inventory_not_ready", Message: notReady.Summary() + "; retry once the inventory finishes.", Action: "Wait for the Kubernetes inventory to finish syncing and retry.", Retryable: true}
	case errors.Is(err, ErrInventoryNotReady):
		return ErrorDetail{Code: "inventory_not_ready", Message: "The Kubernetes inventory has not finished syncing for this resource yet.", Action: "Wait for the Kubernetes inventory to finish syncing and retry.", Retryable: true}
	case errors.As(err, &denied):
		return ErrorDetail{Code: "cluster_permission_denied", Message: "Kubernetes denied " + denied.describe() + ".", Action: "Grant the connector identity the required read-only Kubernetes RBAC permissions.", Retryable: false}
	case errors.Is(err, ErrTimelineContention):
		return ErrorDetail{Code: "timeline_storage_busy", Message: "Timeline storage is busy.", Action: "Retry the current page; existing pagination remains available.", Retryable: true}
	case errors.As(err, &throttled):
		return ErrorDetail{Code: "cluster_throttled", Message: "The Kubernetes API throttled this read.", Action: "Retry after the server-requested delay.", Retryable: true}
	case errors.Is(err, ErrIndexStreamBusy):
		return ErrorDetail{Code: "stream_busy", Message: "The concurrent Kubernetes stream limit has been reached.", Action: "Close another stream and retry.", Retryable: true}
	case errors.Is(err, ErrClusterRequired):
		return ErrorDetail{Code: "cluster_required", Message: "Select a Kubernetes cluster.", Action: "Supply the cluster parameter."}
	case errors.Is(err, ErrClusterNotFound):
		return ErrorDetail{Code: "cluster_not_found", Message: "The Kubernetes cluster is unavailable.", Action: "Select a visible cluster."}
	case errors.Is(err, ErrClusterUnavailable):
		return ErrorDetail{Code: "connector_unreachable", Message: "The cluster did not respond.", Action: "Check the cluster connector configuration.", Retryable: true}
	case errors.Is(err, ErrLogStreamBusy):
		return ErrorDetail{Code: "log_stream_busy", Message: "The concurrent Pod log stream limit has been reached.", Action: "Close another log stream and retry with backoff.", Retryable: true}
	case errors.Is(err, errPreviousUnavailable):
		return ErrorDetail{Code: "previous_unavailable", Message: "Previous container logs are unavailable.", Action: "Select current container logs.", Retryable: false}
	case errors.Is(err, ErrInvalidArguments):
		return ErrorDetail{Code: "invalid_arguments", Message: "The Kubernetes request is missing or has invalid parameters.", Action: "Check the namespace, resource ID, object name, container, and numeric limits.", Retryable: false}
	case errors.Is(err, ErrRequestTooLarge):
		return ErrorDetail{Code: "request_too_large", Message: "The log stream request is larger than this endpoint accepts.", Action: "Resume from a line timestamp instead of carrying the whole resume state in the request.", Retryable: false}
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
