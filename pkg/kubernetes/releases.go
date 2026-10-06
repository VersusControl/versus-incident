package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type HelmRevision struct {
	Revision int       `json:"revision"`
	Status   string    `json:"status"`
	Created  time.Time `json:"created,omitempty"`
}

type HelmRelease struct {
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Current   HelmRevision   `json:"current"`
	History   []HelmRevision `json:"history"`
	Health    string         `json:"health"`
}

type HelmReleaseOptions struct {
	Namespace string
	Status    string
	Limit     int
	Cursor    string
}

type HelmReleasePage struct {
	Items     []HelmRelease    `json:"items"`
	Next      string           `json:"next,omitempty"`
	Truncated bool             `json:"truncated"`
	Partial   []PartialFailure `json:"partial_failures,omitempty"`
}

type helmMetadataList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Namespace         string            `json:"namespace"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
	} `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

// Releases reads Helm revision labels from PartialObjectMetadata only.
func (service *Service) Releases(ctx context.Context, options HelmReleaseOptions) (HelmReleasePage, error) {
	if options.Namespace != "" && !safeSegment(options.Namespace) || options.Status != "" && !safeSegment(options.Status) {
		return HelmReleasePage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudget(ctx)
	defer cancel()
	result := HelmReleasePage{Items: []HelmRelease{}}
	metadata, incomplete, err := service.readHelmMetadataPages(ctx, options.Namespace)
	if err != nil {
		result.Truncated = true
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~secrets", Class: errorClass(err)})
	}
	releases, err := groupHelmMetadata(metadata)
	if err != nil {
		return HelmReleasePage{}, err
	}
	if options.Status != "" {
		filtered := releases[:0]
		for _, release := range releases {
			if strings.EqualFold(release.Current.Status, options.Status) || strings.EqualFold(release.Health, options.Status) {
				filtered = append(filtered, release)
			}
		}
		releases = filtered
	}
	sort.Slice(releases, func(i, j int) bool {
		left, right := helmReleaseRank(releases[i]), helmReleaseRank(releases[j])
		if left != right {
			return left < right
		}
		if releases[i].Namespace != releases[j].Namespace {
			return releases[i].Namespace < releases[j].Namespace
		}
		return releases[i].Name < releases[j].Name
	})
	start, end, next, windowErr := pageWindow(options.Cursor, normalizePageLimit(options.Limit), len(releases))
	if windowErr != nil {
		return HelmReleasePage{}, windowErr
	}
	result.Items = append(result.Items, releases[start:end]...)
	result.Next = next
	result.Truncated = result.Truncated || incomplete || next != ""
	return result, nil
}

// Release returns one label-only Helm release and its bounded revision history.
func (service *Service) Release(ctx context.Context, namespace, name string) (HelmRelease, error) {
	if !safeSegment(namespace) || !safeSegment(name) {
		return HelmRelease{}, ErrInvalidArguments
	}
	metadata, err := service.readHelmMetadata(ctx, namespace, name)
	if err != nil {
		return HelmRelease{}, err
	}
	releases, err := groupHelmMetadata(metadata)
	if err != nil {
		return HelmRelease{}, err
	}
	for _, release := range releases {
		if release.Namespace == namespace && release.Name == name {
			return release, nil
		}
	}
	return HelmRelease{}, ErrNotFound
}

func (service *Service) readHelmMetadata(ctx context.Context, namespace, name string) (helmMetadataList, error) {
	return service.readHelmMetadataPage(ctx, namespace, name, "", 0)
}

func (service *Service) readHelmMetadataPage(ctx context.Context, namespace, name, continuation string, limit int) (helmMetadataList, error) {
	definition, err := service.resolve(ctx, "core~v1~secrets")
	if err != nil {
		return helmMetadataList{}, err
	}
	if !definition.Available {
		return helmMetadataList{}, ErrForbidden
	}
	apiPath, err := resourcePath(definition, namespace, "")
	if err != nil {
		return helmMetadataList{}, err
	}
	selector := "owner=helm"
	if name != "" {
		selector += ",name=" + name
	}
	query := url.Values{"labelSelector": {selector}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(min(limit, maxPageSize)))
	}
	if continuation != "" {
		query.Set("continue", continuation)
	}
	apiPath += "?" + query.Encode()
	payload, err := service.client.get(ctx, apiPath, "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	if err != nil {
		return helmMetadataList{}, err
	}
	var result helmMetadataList
	if err := json.Unmarshal(payload, &result); err != nil {
		return helmMetadataList{}, errors.New("kubernetes: invalid metadata response")
	}
	return result, nil
}

func (service *Service) readHelmMetadataPages(ctx context.Context, namespace string) (helmMetadataList, bool, error) {
	result := helmMetadataList{Items: []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Namespace         string            `json:"namespace"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
	}{}}
	seen := make(map[string]bool)
	continuation := ""
	for range 100 {
		page, err := service.readHelmMetadataPage(ctx, namespace, "", continuation, maxPageSize)
		if err != nil {
			return result, true, err
		}
		result.Items = append(result.Items, page.Items...)
		continuation = page.Metadata.Continue
		if continuation == "" {
			return result, false, nil
		}
		if len(continuation) > 4096 || seen[continuation] || len(page.Items) == 0 {
			return result, true, nil
		}
		seen[continuation] = true
	}
	return result, true, nil
}

func groupHelmMetadata(metadata helmMetadataList) ([]HelmRelease, error) {
	grouped := make(map[string][]HelmRevision)
	for _, item := range metadata.Items {
		labels := item.Metadata.Labels
		if labels["owner"] != "helm" || !safeSegment(labels["name"]) {
			continue
		}
		revision, err := strconv.Atoi(labels["version"])
		if err != nil || revision < 1 {
			continue
		}
		key := item.Metadata.Namespace + "\x00" + labels["name"]
		grouped[key] = append(grouped[key], HelmRevision{Revision: revision, Status: boundString(labels["status"]), Created: item.Metadata.CreationTimestamp})
	}
	result := make([]HelmRelease, 0, len(grouped))
	for key, revisions := range grouped {
		namespace, name, _ := strings.Cut(key, "\x00")
		sort.Slice(revisions, func(i, j int) bool { return revisions[i].Revision > revisions[j].Revision })
		if len(revisions) > 20 {
			revisions = revisions[:20]
		}
		current := revisions[0]
		for _, revision := range revisions {
			if revision.Status == "deployed" {
				current = revision
				break
			}
		}
		health := "healthy"
		latest := revisions[0]
		if latest.Status == "failed" || strings.HasPrefix(latest.Status, "pending-") {
			health = "warning"
		}
		result = append(result, HelmRelease{Namespace: namespace, Name: name, Current: current, History: revisions, Health: health})
	}
	return result, nil
}

func helmReleaseRank(release HelmRelease) int {
	if release.Health != "healthy" {
		return 0
	}
	return 1
}
