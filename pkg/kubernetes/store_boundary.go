package kubernetes

import (
	"context"
	"errors"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// storeFailure is a store outage returned by a store call this package made.
// SetChangeStorage installs the configured provider behind wrapStore, so every
// reader and writer here (inventory, leases, history, timelines, usage) reaches
// the store through one place that marks the outage, and the outage is named
// for the store wherever it surfaces. A failure is named for a component only
// when it came out of that component's own call: a deadline that ran out
// anywhere else is not blamed on the store.
type storeFailure struct{ cause error }

func (failure *storeFailure) Error() string { return failure.cause.Error() }
func (failure *storeFailure) Unwrap() error { return failure.cause }

// markStoreFailure marks err as a store failure. A nil error stays nil and an
// error already marked is returned as it is.
func markStoreFailure(err error) error {
	if err == nil || isStoreFailure(err) {
		return err
	}
	return &storeFailure{cause: err}
}

func isStoreFailure(err error) bool {
	var failure *storeFailure
	return errors.As(err, &failure)
}

// classifyStoreError marks a store outage as a store failure and returns any
// other answer as it is.
func classifyStoreError(err error) error {
	if !storeOutage(err) {
		return err
	}
	return markStoreFailure(err)
}

// storeOutage reports whether err says the store did not answer: the classes
// historyStoreError names timeout or unavailable. A call cancelled because its
// caller stopped waiting, and an answer the store gave (storage.IsAnswer), are
// not outages.
func storeOutage(err error) bool {
	if err == nil || storage.IsAnswer(err) {
		return false
	}
	if errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	classified := historyStoreError(err)
	return errors.Is(classified, errHistoryTimeout) || errors.Is(classified, errHistoryUnavailable)
}

// wrapStore returns provider with every call's error classified by
// classifyStoreError (through storage.Observe, which keeps the provider's
// capabilities). A provider that is already wrapped is returned unchanged.
func wrapStore(provider storage.Provider) storage.Provider {
	return storage.Observe(provider, classifyStoreError)
}

// unwrapStore returns the provider a wrapped store stands in front of.
func unwrapStore(provider storage.Provider) storage.Provider {
	return storage.Unobserved(provider)
}
