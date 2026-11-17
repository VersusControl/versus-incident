// Package probe asks each AWS service in the catalog one smallest-page read
// and reports what it found. Probes never write, never echo provider error
// text, and never turn a throttling or transport failure into a denial.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// Status is the word the settings grid renders for one service.
type Status string

const (
	// StatusReadable means the call succeeded and returned at least one item.
	StatusReadable Status = "readable"
	// StatusNotUsed means the call succeeded and the account has no items.
	StatusNotUsed Status = "not_used"
	// StatusDenied means AWS refused the read; the result carries fix actions.
	StatusDenied Status = "denied"
	// StatusNotOnPlan means the account is not subscribed to the service.
	StatusNotOnPlan Status = "not_on_plan"
	// StatusNotChecked means throttling, a timeout, or a transport failure
	// prevented an answer. It is never treated as a denial or as zero items.
	StatusNotChecked Status = "not_checked"
)

// ErrorCode is the closed vocabulary carried by a result. It never contains
// provider text.
const (
	// CodeDenied is the code for an authorization failure.
	CodeDenied = "denied"
	// CodeNotOnPlan is the code for a service the account is not subscribed to.
	CodeNotOnPlan = "not_on_plan"
	// CodeNotInRegion is the code for a service with no endpoint in the
	// region probed: nothing can be there, so it is not a failure.
	CodeNotInRegion = "not_available_in_region"
)

// The remaining codes reuse the awsauth vocabulary so callers render one set
// of messages for credentials and probes.
var (
	// CodeThrottled is the code for a throttling reply or an active backoff.
	CodeThrottled = awsauth.ErrThrottled.Error()
	// CodeTimeout is the code for a deadline or a request timeout.
	CodeTimeout = awsauth.ErrTimeout.Error()
	// CodeNetwork is the code for any failure that is not clearly an
	// authorization, plan, or throttling condition.
	CodeNetwork = awsauth.ErrNetwork.Error()
	// CodeRegionDisabled is the code for a region the account cannot use.
	CodeRegionDisabled = awsauth.ErrRegionDisabled.Error()
)

// ErrNoDeniedServices means a fix policy was requested when no service is
// denied.
var ErrNoDeniedServices = errors.New("no denied services")

// requestTimeout bounds one probe independent of the caller's deadline.
const requestTimeout = 10 * time.Second

// Result is one service's probe outcome.
type Result struct {
	Service    string   `json:"service"`
	Status     Status   `json:"status"`
	Items      int      `json:"items"`
	ErrorCode  string   `json:"error_code,omitempty"`
	FixActions []string `json:"fix_actions,omitempty"`
	Retryable  bool     `json:"retryable"`
}

// Prober runs the catalog's read-only probes for one account.
type Prober struct {
	accountID   string
	credentials aws.CredentialsProvider
	limiter     *limit.Manager
	httpClient  aws.HTTPClient
}

// Option adjusts a Prober.
type Option func(*Prober)

// WithHTTPClient injects the transport used for every service call. Tests use
// it to answer without the network.
func WithHTTPClient(client aws.HTTPClient) Option {
	return func(prober *Prober) {
		if client != nil {
			prober.httpClient = client
		}
	}
}

// New builds a Prober. A nil limiter disables rate limiting and metering; the
// settings path always supplies one.
func New(accountID string, credentials aws.CredentialsProvider, limiter *limit.Manager, options ...Option) *Prober {
	prober := &Prober{
		accountID:   accountID,
		credentials: credentials,
		limiter:     limiter,
		httpClient:  awsauth.NewHTTPClient(),
	}
	for _, option := range options {
		option(prober)
	}
	return prober
}

// ProbeAll runs every catalog probe once. Each result is independent, so a
// denial or a throttled service never hides the rest of the grid.
func (prober *Prober) ProbeAll(ctx context.Context, homeRegion string) []Result {
	services := catalog.Services()
	results := make([]Result, 0, len(services))
	for _, entry := range services {
		results = append(results, prober.Probe(ctx, entry, homeRegion))
	}
	return results
}

// Probe runs the smallest read for one service. Global services use the
// us-east-1 limiter; regional services use the account's home region.
func (prober *Prober) Probe(ctx context.Context, entry catalog.Service, homeRegion string) Result {
	result := Result{Service: entry.ID}
	region := entry.Region(homeRegion)

	run, ok := probes[entry.ID]
	if !ok {
		return notChecked(result, CodeNetwork, false)
	}
	if prober.credentials == nil {
		return notChecked(result, awsauth.ErrNoServerIdentity.Error(), false)
	}

	if prober.limiter != nil {
		if err := prober.limiter.Wait(ctx, prober.accountID, region); err != nil {
			if errors.Is(err, limit.ErrBackoff) {
				return notChecked(result, CodeThrottled, true)
			}
			return notChecked(result, timeoutOrNetwork(err), true)
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	items, err := run(probeCtx, env{
		credentials: prober.credentials,
		httpClient:  prober.httpClient,
		region:      region,
	})
	if err != nil {
		status, code, retryable := classify(err)
		if prober.limiter != nil {
			if code == CodeThrottled {
				prober.limiter.Throttled(prober.accountID, region)
			} else {
				prober.limiter.Succeeded(prober.accountID, region)
			}
		}
		result.Status = status
		result.ErrorCode = code
		result.Retryable = retryable
		if status == StatusDenied {
			result.FixActions = append([]string(nil), entry.FixActions...)
		}
		return result
	}

	if prober.limiter != nil {
		prober.limiter.Succeeded(prober.accountID, region)
	}
	result.Items = items
	if items > 0 {
		result.Status = StatusReadable
	} else {
		result.Status = StatusNotUsed
	}
	return result
}

func notChecked(result Result, code string, retryable bool) Result {
	result.Status = StatusNotChecked
	result.ErrorCode = code
	result.Retryable = retryable
	return result
}

// classify maps a probe failure onto a status and the closed vocabulary. It
// never inspects or forwards a provider message.
func classify(err error) (Status, string, bool) {
	if err == nil {
		return StatusNotChecked, CodeNetwork, false
	}
	if awsauth.ServiceNotInRegion(err) {
		return StatusNotUsed, CodeNotInRegion, false
	}

	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation",
			"AuthorizationError", "AuthorizationErrorException", "NotAuthorized",
			"Forbidden", "MissingAuthenticationTokenException":
			return StatusDenied, CodeDenied, false
		case "SubscriptionRequiredException":
			return StatusNotOnPlan, CodeNotOnPlan, false
		case "Throttling", "ThrottlingException", "RequestLimitExceeded",
			"TooManyRequestsException", "SlowDown":
			return StatusNotChecked, CodeThrottled, true
		case "RegionDisabledException", "RegionDisabled":
			return StatusNotChecked, CodeRegionDisabled, false
		case "RequestTimeout", "RequestTimeoutException":
			return StatusNotChecked, CodeTimeout, true
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return StatusNotChecked, CodeTimeout, true
	}
	if code := awsauth.ErrorCode(err); code != "" {
		switch code {
		case awsauth.ErrThrottled.Error():
			return StatusNotChecked, code, true
		case awsauth.ErrTimeout.Error(), awsauth.ErrNetwork.Error():
			return StatusNotChecked, code, true
		default:
			return StatusNotChecked, code, false
		}
	}
	return StatusNotChecked, CodeNetwork, true
}

func timeoutOrNetwork(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CodeTimeout
	}
	if errors.Is(err, awsauth.ErrTimeout) {
		return CodeTimeout
	}
	return CodeNetwork
}

// BuildFixPolicy renders a copyable least-privilege IAM policy statement for
// the denied services. Actions come from the catalog so the panel, the policy
// and the probe cannot drift.
func BuildFixPolicy(denied []Result) (string, error) {
	actions := make(map[string]struct{})
	for _, result := range denied {
		if result.Status != StatusDenied {
			continue
		}
		service, ok := catalog.ByID(result.Service)
		if !ok {
			continue
		}
		for _, action := range service.FixActions {
			action = strings.TrimSpace(action)
			if action != "" {
				actions[action] = struct{}{}
			}
		}
	}
	if len(actions) == 0 {
		return "", ErrNoDeniedServices
	}

	sorted := make([]string, 0, len(actions))
	for action := range actions {
		sorted = append(sorted, action)
	}
	sort.Strings(sorted)

	policy := struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect   string   `json:"Effect"`
			Action   []string `json:"Action"`
			Resource string   `json:"Resource"`
		} `json:"Statement"`
	}{Version: "2012-10-17"}
	policy.Statement = append(policy.Statement, struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource string   `json:"Resource"`
	}{Effect: "Allow", Action: sorted, Resource: "*"})

	encoded, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
