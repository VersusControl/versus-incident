package graylog

import (
	"context"
	"time"
)

func (service *Service) ReadWindow(ctx context.Context, request ReadRequest, from, to time.Time) (ReadResult, error) {
	if from.IsZero() || !to.After(from) || to.Sub(from) > 2*time.Hour || to.After(time.Now().Add(time.Minute)) {
		return ReadResult{}, ErrInvalidArgument
	}
	copy := *service
	copy.now = func() time.Time { return to }
	request.Lookback = to.Sub(from)
	return copy.Read(ctx, request)
}
