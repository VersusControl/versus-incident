//go:build harness_fakeaws

package main

import (
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// harnessFakeAWSEnv names the fake AWS endpoint the harness build sends every
// AWS call to. This file is compiled only with the harness_fakeaws build tag;
// release builds have neither the variable nor the redirect.
const harnessFakeAWSEnv = "VERSUS_HARNESS_FAKEAWS_URL"

// awsHarnessHTTPClient returns a client that rewrites every AWS request to the
// fake endpoint while keeping path, query, headers and signing intact, or nil
// when the variable is unset or not an http(s) URL.
func awsHarnessHTTPClient() aws.HTTPClient {
	base, err := url.Parse(os.Getenv(harnessFakeAWSEnv))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil
	}
	return &http.Client{Transport: harnessRedirect{base: base, next: http.DefaultTransport}}
}

type harnessRedirect struct {
	base *url.URL
	next http.RoundTripper
}

func (redirect harnessRedirect) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	target := *request.URL
	target.Scheme = redirect.base.Scheme
	target.Host = redirect.base.Host
	clone.URL = &target
	clone.Host = redirect.base.Host
	return redirect.next.RoundTrip(clone)
}
