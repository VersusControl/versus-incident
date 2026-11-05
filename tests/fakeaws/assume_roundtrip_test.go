package fakeaws_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestAssumeDeniedScenariosMapToClosedCodes proves the fixture speaks real STS
// errors: every selector round-trips through the product assume path to its
// closed-vocabulary code, never to the catch-all network code.
func TestAssumeDeniedScenariosMapToClosedCodes(t *testing.T) {
	cases := []struct {
		selector string
		want     error
	}{
		{fakeaws.AssumeTrustDenied, awsauth.ErrTrustDenied},
		{fakeaws.AssumeExternalIDMismatch, awsauth.ErrExternalIDMismatch},
		{fakeaws.AssumeRoleNotFound, awsauth.ErrRoleNotFound},
		{fakeaws.AssumeSessionDurationInvalid, awsauth.ErrSessionDurationInvalid},
	}
	base := credentials.NewStaticCredentialsProvider("AKIAVERSUS", "versus-secret", "")
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			server := fakeaws.New(t, "assume-denied")
			provider, err := awsauth.AssumeRoleWithHTTPClient(context.Background(), base, awsauth.RoleSpec{
				RoleARN:     "arn:aws:iam::444455556666:role/versus-reader",
				ExternalID:  "vrs-ext-abcdefgh12345678",
				SessionName: tc.selector,
			}, "us-east-1", server.HTTPClient())
			if err != nil {
				t.Fatalf("assume setup: %v", err)
			}
			_, err = provider.Retrieve(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("retrieve err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, awsauth.ErrNetwork) {
				t.Fatalf("%s mapped to network", tc.selector)
			}
			if got := awsauth.ErrorCode(err); got != tc.want.Error() {
				t.Fatalf("error code = %q, want %q", got, tc.want.Error())
			}
		})
	}
}
