package cloudwatchlogs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	endpoints "github.com/aws/smithy-go/endpoints"
)

// A 100-event page with typical 2 KiB messages fits; oversized upstream pages remain bounded.
const maxResponseBytes = 512 * 1024

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z0-9]+){1,3}-[0-9]+$`)

type pinnedResolver struct{}

func (pinnedResolver) ResolveEndpoint(ctx context.Context, params cloudwatchlogs.EndpointParameters) (endpoints.Endpoint, error) {
	params.Endpoint = nil
	disabled := false
	params.UseDualStack = &disabled
	params.UseFIPS = &disabled
	return cloudwatchlogs.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, params)
}

type pinnedClient struct {
	client aws.HTTPClient
	host   string
}

func (client pinnedClient) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != client.host {
		return nil, ErrScope
	}
	response, err := client.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxResponseBytes {
		response.Body.Close()
		return nil, ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, ErrResponse
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

func NewToolAPI(ctx context.Context, region string) (API, error) {
	return NewToolAPIWithHTTPClient(ctx, region, nil)
}

func NewToolAPIWithHTTPClient(ctx context.Context, region string, client aws.HTTPClient) (API, error) {
	if !regionPattern.MatchString(region) {
		return nil, ErrScope
	}
	resolved, err := (pinnedResolver{}).ResolveEndpoint(ctx, cloudwatchlogs.EndpointParameters{Region: &region})
	if err != nil || resolved.URI.Scheme != "https" || resolved.URI.Host == "" {
		return nil, ErrScope
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithRetryMaxAttempts(1))
	if err != nil {
		return nil, ErrScope
	}
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return cloudwatchlogs.NewFromConfig(cfg, func(options *cloudwatchlogs.Options) {
		options.BaseEndpoint = nil
		options.EndpointResolverV2 = pinnedResolver{}
		options.HTTPClient = pinnedClient{client: client, host: resolved.URI.Host}
		options.RetryMaxAttempts = 1
	}), nil
}
