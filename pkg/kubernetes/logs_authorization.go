package kubernetes

import (
	"context"
	"time"
)

type logStreamAuthorizationKey struct{}

// WithLogStreamAuthorization installs a request-scoped access revalidator.
// The callback must honor cancellation and return only a safe authorization
// error. It is evaluated before admission and periodically even on idle reads.
func WithLogStreamAuthorization(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, logStreamAuthorizationKey{}, check)
}

func checkLogStreamAuthorization(ctx context.Context) error {
	check, _ := ctx.Value(logStreamAuthorizationKey{}).(func(context.Context) error)
	if check == nil {
		return ctx.Err()
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return check(bounded)
}

func logStreamAuthorizationContext(parent context.Context) (context.Context, func(), error) {
	if err := checkLogStreamAuthorization(parent); err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	check, _ := parent.Value(logStreamAuthorizationKey{}).(func(context.Context) error)
	if check == nil {
		return ctx, cancel, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if checkLogStreamAuthorization(ctx) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}

// logStreamAccessContext observes rotating credential identity independently
// of log traffic or other API requests. Authorization and credential checks
// cancel the same parent, so idle fan-in children cannot outlive access.
func (service *Service) logStreamAccessContext(parent context.Context) (context.Context, func(), error) {
	authorized, stopAuthorization, err := logStreamAuthorizationContext(parent)
	if err != nil {
		return nil, nil, err
	}
	if err := service.validateCredential(authorized); err != nil {
		stopAuthorization()
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(authorized)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 5*time.Second)
				err := service.validateCredential(check)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done; stopAuthorization() }, nil
}
