package kubernetes

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type TrafficQuery struct {
	Namespace string
	Source    string
	Window    string
}

type TrafficWorkload struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

// TrafficEdge leaves unavailable measurements nil so they serialize as null.
type TrafficEdge struct {
	From        TrafficWorkload `json:"from"`
	To          TrafficWorkload `json:"to"`
	RatePerSec  *float64        `json:"rate_per_sec"`
	ErrorRate   *float64        `json:"error_rate"`
	P95Ms       *float64        `json:"p95_ms"`
	BytesPerSec *float64        `json:"bytes_per_sec"`
}

type Traffic struct {
	Available  bool          `json:"available"`
	Reason     string        `json:"reason,omitempty"`
	Source     string        `json:"source,omitempty"`
	Window     string        `json:"window"`
	ObservedAt time.Time     `json:"observed_at,omitempty"`
	Edges      []TrafficEdge `json:"edges,omitempty"`
	Unmapped   int           `json:"unmapped"`
	External   []string      `json:"external,omitempty"`
	Truncated  bool          `json:"truncated"`
}

type TrafficContributor interface {
	ReadTraffic(context.Context, TrafficQuery) (Traffic, error)
}

type trafficProviderState struct {
	mu       sync.RWMutex
	provider TrafficContributor
}

// SetTrafficContributor installs an optional traffic reader without coupling
// the OSS Kubernetes service to any particular telemetry implementation.
func (service *Service) SetTrafficContributor(provider TrafficContributor) {
	if service == nil || service.traffic == nil {
		return
	}
	service.traffic.mu.Lock()
	service.traffic.provider = provider
	service.traffic.mu.Unlock()
}

func (service *Service) Traffic(ctx context.Context, query TrafficQuery) (Traffic, error) {
	if service == nil || service.traffic == nil || query.Namespace != "" && !safeSegment(query.Namespace) || !safeTrafficSource(query.Source) {
		return Traffic{}, ErrInvalidArguments
	}
	if query.Window == "" {
		query.Window = "15m"
	}
	switch query.Window {
	case "5m", "15m", "1h":
	default:
		return Traffic{}, ErrInvalidArguments
	}
	service.traffic.mu.RLock()
	provider := service.traffic.provider
	service.traffic.mu.RUnlock()
	if provider == nil {
		return Traffic{Available: false, Reason: "no flow source connected; configure a supported traffic contributor", Window: query.Window}, nil
	}
	result, err := provider.ReadTraffic(ctx, query)
	if err != nil {
		return Traffic{}, err
	}
	if !result.Available {
		result.Edges = nil
		result.External = nil
		if result.Reason == "" {
			result.Reason = "no flow source connected"
		}
		return result, nil
	}
	for index := range result.Edges {
		result.Edges[index].RatePerSec = validTrafficMetric(result.Edges[index].RatePerSec, math.Inf(1))
		result.Edges[index].ErrorRate = validTrafficMetric(result.Edges[index].ErrorRate, 1)
		result.Edges[index].P95Ms = validTrafficMetric(result.Edges[index].P95Ms, math.Inf(1))
		result.Edges[index].BytesPerSec = validTrafficMetric(result.Edges[index].BytesPerSec, math.Inf(1))
	}
	sort.SliceStable(result.Edges, func(i, j int) bool {
		leftErrorRate, rightErrorRate := result.Edges[i].ErrorRate, result.Edges[j].ErrorRate
		if leftErrorRate == nil || rightErrorRate == nil {
			if leftErrorRate != rightErrorRate {
				return leftErrorRate != nil
			}
		} else if *leftErrorRate != *rightErrorRate {
			return *leftErrorRate > *rightErrorRate
		}
		if result.Edges[i].From.Namespace != result.Edges[j].From.Namespace {
			return result.Edges[i].From.Namespace < result.Edges[j].From.Namespace
		}
		if result.Edges[i].From.Name != result.Edges[j].From.Name {
			return result.Edges[i].From.Name < result.Edges[j].From.Name
		}
		if result.Edges[i].To.Namespace != result.Edges[j].To.Namespace {
			return result.Edges[i].To.Namespace < result.Edges[j].To.Namespace
		}
		return result.Edges[i].To.Name < result.Edges[j].To.Name
	})
	sort.Strings(result.External)
	if len(result.Edges) > 100 {
		result.Edges = result.Edges[:100]
		result.Truncated = true
	}
	if len(result.External) > 20 {
		result.External = result.External[:20]
		result.Truncated = true
	}
	if result.Unmapped < 0 {
		result.Unmapped = 0
	}
	return result, nil
}

func safeTrafficSource(source string) bool {
	if len(source) > 64 || strings.TrimSpace(source) != source {
		return false
	}
	for _, character := range source {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}

func validTrafficMetric(value *float64, maximum float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return nil
	}
	if *value > maximum {
		bounded := maximum
		return &bounded
	}
	return value
}
