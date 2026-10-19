package splunk

import (
	"context"
	"sort"
	"time"
)

func (service *Service) ReadWindow(ctx context.Context, filters map[string]string, from, to time.Time, limit int, prefixes ...map[string]string) (ReadResult, error) {
	return service.ReadWindowWithFields(ctx, filters, from, to, limit, nil, prefixes...)
}

func (service *Service) ReadWindowWithStringFields(ctx context.Context, filters map[string]string, from, to time.Time, limit int, fields []string, prefixes ...map[string]string) (ReadResult, error) {
	copy := *service
	copy.stringFields = append([]string(nil), fields...)
	for field := range filters {
		copy.stringFields = append(copy.stringFields, field)
	}
	for _, prefix := range prefixes {
		for field := range prefix {
			copy.stringFields = append(copy.stringFields, field)
		}
	}
	return copy.ReadWindowWithFields(ctx, filters, from, to, limit, fields, prefixes...)
}

func (service *Service) ReadWindowWithFields(ctx context.Context, filters map[string]string, from, to time.Time, limit int, fields []string, prefixes ...map[string]string) (ReadResult, error) {
	if from.IsZero() || !to.After(from) || to.Sub(from) > 2*time.Hour || to.After(time.Now().Add(time.Minute)) || len(filters) > 3 || limit < 1 || limit > MaximumLimit {
		return ReadResult{}, ErrInvalidArgument
	}
	copy := *service
	copy.now = func() time.Time { return to }
	copy.scope = append([]clause(nil), service.scope...)
	copy.extraProjection = append([]string(nil), service.extraProjection...)
	if len(fields) > 12 {
		return ReadResult{}, ErrInvalidArgument
	}
	for _, field := range fields {
		if !outputFieldPattern.MatchString(field) {
			return ReadResult{}, ErrInvalidArgument
		}
		copy.extraProjection = append(copy.extraProjection, field)
	}
	keys := []string{}
	for key, value := range filters {
		if !outputFieldPattern.MatchString(key) || key == "index" || key == "sourcetype" || !literal.MatchString(value) {
			return ReadResult{}, ErrInvalidArgument
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		copy.scope = append(copy.scope, clause{key, filters[key]})
	}
	for _, key := range keys {
		copy.extraProjection = append(copy.extraProjection, key)
	}
	if len(prefixes) > 1 {
		return ReadResult{}, ErrInvalidArgument
	}
	if len(prefixes) == 1 && len(prefixes[0])+len(filters) > 3 {
		return ReadResult{}, ErrInvalidArgument
	}
	for _, prefix := range prefixes {
		for key, value := range prefix {
			if !outputFieldPattern.MatchString(key) || key == "index" || key == "sourcetype" || !literal.MatchString(value) {
				return ReadResult{}, ErrInvalidArgument
			}
			copy.scope = append(copy.scope, clause{key, value + "-*"})
			copy.extraProjection = append(copy.extraProjection, key)
		}
	}
	return copy.Read(ctx, ReadRequest{Lookback: to.Sub(from), Limit: limit})
}
