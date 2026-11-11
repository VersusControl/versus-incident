//go:build !harness_fakeaws

package main

import "github.com/aws/aws-sdk-go-v2/aws"

// awsHarnessHTTPClient is always nil in a release build: AWS calls go to AWS.
func awsHarnessHTTPClient() aws.HTTPClient { return nil }
