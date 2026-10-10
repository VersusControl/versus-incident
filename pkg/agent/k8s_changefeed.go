package agent

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type kubernetesChangeFeed struct {
	service *kubernetes.Service
}

type kubernetesRegistryChangeFeed struct {
	registry *kubernetes.ServiceRegistry
	scope    tenancy.OrgScope
}

func newKubernetesRegistryChangeFeed(registry *kubernetes.ServiceRegistry, scope tenancy.OrgScope) commontools.ChangeFeed {
	if registry == nil || len(registry.Clusters()) == 0 {
		return nil
	}
	return kubernetesRegistryChangeFeed{registry: registry, scope: scope.Normalized()}
}

func (feed kubernetesRegistryChangeFeed) Changes(ctx context.Context, since time.Time) ([]commontools.ChangeRecord, error) {
	result := []commontools.ChangeRecord{}
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return result, nil
	}
	orgID := feed.scope.Write
	if scope, ok := tenancy.ContextOrgScope(ctx); ok {
		orgID = scope.Write
	}
	for _, info := range feed.registry.Clusters() {
		if !core.CallerClusterAllowed(ctx, info.ID) {
			continue
		}
		service, err := feed.registry.ResolveCluster(orgID, info.ID)
		if err != nil {
			continue
		}
		changes, err := (kubernetesChangeFeed{service: service}).Changes(ctx, since)
		if err == nil {
			result = append(result, changes...)
		}
	}
	return result, nil
}

func newKubernetesChangeFeed(service *kubernetes.Service) commontools.ChangeFeed {
	if service == nil {
		return nil
	}
	return kubernetesChangeFeed{service: service}
}

func (feed kubernetesChangeFeed) Changes(ctx context.Context, since time.Time) ([]commontools.ChangeRecord, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return []commontools.ChangeRecord{}, nil
	}
	if feed.service == nil || !core.CallerClusterAllowed(ctx, feed.service.Scope().ClusterID) {
		return nil, nil
	}
	page, err := feed.service.Changes(ctx, kubernetes.ChangeQuery{Since: since, Until: time.Now().UTC(), Limit: 500})
	if err != nil {
		return nil, err
	}
	result := make([]commontools.ChangeRecord, 0, len(page.Items))
	for _, change := range page.Items {
		var summary strings.Builder
		summary.WriteString(change.Kind)
		summary.WriteByte('/')
		summary.WriteString(change.Name)
		summary.WriteString(": ")
		summary.WriteString(string(change.Type))
		for _, field := range change.Fields {
			summary.WriteString("; ")
			summary.WriteString(field.Path)
			summary.WriteString(" ")
			summary.WriteString(field.From)
			summary.WriteString(" -> ")
			summary.WriteString(field.To)
		}
		ref := "k8s://" + url.PathEscape(change.Cluster) + "/" + url.PathEscape(change.Namespace) + "/" + url.PathEscape(change.Kind) + "/" + url.PathEscape(change.Name)
		result = append(result, commontools.ChangeRecord{Timestamp: change.At, Service: change.Service, Kind: "k8s_" + string(change.Type), Summary: summary.String(), Ref: ref})
	}
	return result, nil
}

type mergedChangeFeed struct {
	feeds []commontools.ChangeFeed
}

func mergeChangeFeeds(feeds ...commontools.ChangeFeed) commontools.ChangeFeed {
	merged := mergedChangeFeed{}
	for _, feed := range feeds {
		if feed != nil {
			merged.feeds = append(merged.feeds, feed)
		}
	}
	if len(merged.feeds) == 0 {
		return nil
	}
	return merged
}

func (feed mergedChangeFeed) Changes(ctx context.Context, since time.Time) ([]commontools.ChangeRecord, error) {
	result := make([]commontools.ChangeRecord, 0)
	var failures []error
	succeeded := false
	for _, source := range feed.feeds {
		changes, err := source.Changes(ctx, since)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		succeeded = true
		result = append(result, changes...)
	}
	if !succeeded && len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Timestamp.Equal(result[right].Timestamp) {
			return result[left].Service < result[right].Service
		}
		return result[left].Timestamp.After(result[right].Timestamp)
	})
	return result, nil
}
