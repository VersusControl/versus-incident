package cost

import (
	"bytes"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// Billing APIs are global and served from us-east-1 in the aws partition.
const (
	billingRegion   = "us-east-1"
	costExplorerURL = "ce.us-east-1.amazonaws.com"
	budgetsHost     = "budgets.amazonaws.com"
	optimizationURL = "cost-optimization-hub.us-east-1.amazonaws.com"
)

// maxResponseBytes bounds one page. Daily cost pages for a large
// Organization stay well below it; a larger body stops the call.
const maxResponseBytes = 8 << 20

// env carries one payer's credentials and transport.
type env struct {
	credentials aws.CredentialsProvider
	httpClient  aws.HTTPClient
}

// config builds a client configuration without loading ambient providers:
// credentials always come from the payer's assumed reader role, and the SDK
// never retries (the next scheduled refresh tries again).
func (e env) config() aws.Config {
	return aws.Config{
		Region:           billingRegion,
		Credentials:      awsauth.MarkCredentialFailures(e.credentials),
		HTTPClient:       e.httpClient,
		RetryMaxAttempts: 1,
	}
}

func (e env) costExplorer() *costexplorer.Client {
	endpoint := "https://" + costExplorerURL
	return costexplorer.NewFromConfig(e.config(), func(o *costexplorer.Options) {
		o.BaseEndpoint = &endpoint
		o.HTTPClient = pinnedHTTPClient{client: e.httpClient, host: costExplorerURL}
	})
}

func (e env) budgets() *budgets.Client {
	endpoint := "https://" + budgetsHost
	return budgets.NewFromConfig(e.config(), func(o *budgets.Options) {
		o.BaseEndpoint = &endpoint
		o.HTTPClient = pinnedHTTPClient{client: e.httpClient, host: budgetsHost}
	})
}

func (e env) optimizationHub() *costoptimizationhub.Client {
	endpoint := "https://" + optimizationURL
	return costoptimizationhub.NewFromConfig(e.config(), func(o *costoptimizationhub.Options) {
		o.BaseEndpoint = &endpoint
		o.HTTPClient = pinnedHTTPClient{client: e.httpClient, host: optimizationURL}
	})
}

// pinnedHTTPClient rejects any request that leaves the expected host or
// TLS, and caps the response body.
type pinnedHTTPClient struct {
	client aws.HTTPClient
	host   string
}

func (pinned pinnedHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != pinned.host {
		return nil, awsauth.ErrNetwork
	}
	response, err := pinned.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxResponseBytes {
		response.Body.Close()
		return nil, awsauth.ErrNetwork
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, awsauth.ErrNetwork
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
