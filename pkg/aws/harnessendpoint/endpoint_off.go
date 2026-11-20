//go:build !harness_awsendpoint && !harness_fakeaws

package harnessendpoint

import "github.com/aws/aws-sdk-go-v2/aws"

// HTTPClient is always nil in a release build, which keeps the hardened
// default AWS transport.
func HTTPClient() aws.HTTPClient { return nil }
