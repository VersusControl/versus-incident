package awsauth

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// credentialsLoadFailure is a provider that could not load credentials at
// all: no identity on the host, an unreachable metadata or token endpoint, a
// failed role assumption. It reads as ErrNoServerIdentity and hides the
// provider's own error, so a failure whose cause is the credentials is never
// classified by its transport symptom (a timeout or a network fault).
type credentialsLoadFailure struct{}

func (credentialsLoadFailure) Error() string { return ErrNoServerIdentity.Error() }
func (credentialsLoadFailure) Unwrap() error { return ErrNoServerIdentity }

// markedCredentials reports every load failure of its provider as a
// credentials cause.
type markedCredentials struct{ provider aws.CredentialsProvider }

func (marked markedCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	credentials, err := marked.provider.Retrieve(ctx)
	if err != nil && ctx.Err() == nil {
		// The caller's own deadline or cancellation is the work's
		// timeout, not a credentials failure, and keeps its error.
		return aws.Credentials{}, credentialsLoadFailure{}
	}
	return credentials, err
}

// MarkCredentialFailures wraps provider so that, when it cannot load
// credentials at all, the request fails with an error that is
// errors.Is(ErrNoServerIdentity), whatever the provider returned. A nil or
// anonymous provider is returned unchanged.
func MarkCredentialFailures(provider aws.CredentialsProvider) aws.CredentialsProvider {
	if provider == nil || aws.IsCredentialsProvider(provider, (*aws.AnonymousCredentials)(nil)) {
		return provider
	}
	if _, marked := provider.(markedCredentials); marked {
		return provider
	}
	return markedCredentials{provider: provider}
}
