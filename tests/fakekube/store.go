package fakekube

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrInvalidRequest  = errors.New("fakekube: invalid request")
	ErrExpiredContinue = errors.New("fakekube: continuation snapshot expired")
)

type objectKey struct {
	resource  string
	namespace string
	name      string
}

type storedObject struct {
	key  objectKey
	rv   uint64
	body json.RawMessage
}

type Store struct {
	mu      sync.RWMutex
	objects map[objectKey]storedObject
	nextRV  uint64
}

type ListResult struct {
	ResourceVersion string            `json:"resourceVersion"`
	Continue        string            `json:"continue,omitempty"`
	Items           []json.RawMessage `json:"items"`
}

func NewStore() *Store {
	return &Store{objects: make(map[objectKey]storedObject)}
}

func (store *Store) Upsert(resource, namespace, name string, body json.RawMessage) error {
	if store == nil || !safeSegment(resource) || !safeOptionalSegment(namespace) || !safeSegment(name) || !json.Valid(body) || len(body) > 1<<20 {
		return ErrInvalidRequest
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return ErrInvalidRequest
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		metadata = make(map[string]any)
		object["metadata"] = metadata
	}
	metadata["name"] = name
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.nextRV++
	metadata["resourceVersion"] = strconv.FormatUint(store.nextRV, 10)
	encoded, err := json.Marshal(object)
	if err != nil {
		return ErrInvalidRequest
	}
	key := objectKey{resource: resource, namespace: namespace, name: name}
	store.objects[key] = storedObject{key: key, rv: store.nextRV, body: append(json.RawMessage(nil), encoded...)}
	return nil
}

func (store *Store) Delete(resource, namespace, name string) bool {
	if store == nil {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	key := objectKey{resource: resource, namespace: namespace, name: name}
	if _, exists := store.objects[key]; !exists {
		return false
	}
	delete(store.objects, key)
	store.nextRV++
	return true
}

func (store *Store) Get(resource, namespace, name string) (json.RawMessage, bool) {
	if store == nil || !safeSegment(resource) || !safeOptionalSegment(namespace) || !safeSegment(name) {
		return nil, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	object, exists := store.objects[objectKey{resource: resource, namespace: namespace, name: name}]
	if !exists {
		return nil, false
	}
	return append(json.RawMessage(nil), object.body...), true
}

func (store *Store) List(resource, namespace string, query url.Values) (ListResult, error) {
	if store == nil || !safeSegment(resource) || !safeOptionalSegment(namespace) {
		return ListResult{}, ErrInvalidRequest
	}
	limit := 500
	if rawLimit := query.Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 5000 {
			return ListResult{}, ErrInvalidRequest
		}
		limit = parsed
	}
	labelSelector, err := parseSelector(query.Get("labelSelector"))
	if err != nil {
		return ListResult{}, err
	}
	fieldSelector, err := parseSelector(query.Get("fieldSelector"))
	if err != nil {
		return ListResult{}, err
	}
	start := 0
	var snapshotVersion uint64
	if cursor := query.Get("continue"); cursor != "" {
		version, offset, valid := strings.Cut(cursor, ":")
		if !valid {
			return ListResult{}, ErrInvalidRequest
		}
		snapshotVersion, err = strconv.ParseUint(version, 10, 64)
		if err != nil {
			return ListResult{}, ErrInvalidRequest
		}
		start, err = strconv.Atoi(offset)
		if err != nil || start < 0 {
			return ListResult{}, ErrInvalidRequest
		}
	}
	store.mu.RLock()
	resourceVersion := store.nextRV
	if query.Get("continue") != "" && snapshotVersion != resourceVersion {
		store.mu.RUnlock()
		return ListResult{}, ErrExpiredContinue
	}
	if query.Get("continue") == "" {
		snapshotVersion = resourceVersion
	}
	objects := make([]storedObject, 0)
	for _, object := range store.objects {
		if object.key.resource != resource || (namespace != "" && object.key.namespace != namespace) {
			continue
		}
		if matchesSelectors(object.body, labelSelector, fieldSelector) {
			objects = append(objects, object)
		}
	}
	store.mu.RUnlock()
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].key.namespace != objects[j].key.namespace {
			return objects[i].key.namespace < objects[j].key.namespace
		}
		return objects[i].key.name < objects[j].key.name
	})
	if start > len(objects) {
		return ListResult{}, ErrInvalidRequest
	}
	end := min(start+limit, len(objects))
	result := ListResult{ResourceVersion: strconv.FormatUint(resourceVersion, 10), Items: make([]json.RawMessage, 0, end-start)}
	for _, object := range objects[start:end] {
		result.Items = append(result.Items, append(json.RawMessage(nil), object.body...))
	}
	if end < len(objects) {
		result.Continue = strconv.FormatUint(snapshotVersion, 10) + ":" + strconv.Itoa(end)
	}
	return result, nil
}

func (store *Store) Count(resource string) int {
	if store == nil {
		return 0
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	count := 0
	for key := range store.objects {
		if key.resource == resource {
			count++
		}
	}
	return count
}

func (store *Store) ResourceVersion() string {
	if store == nil {
		return "0"
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return strconv.FormatUint(store.nextRV, 10)
}

func parseSelector(value string) (map[string]string, error) {
	result := make(map[string]string)
	if value == "" {
		return result, nil
	}
	if len(value) > 4096 {
		return nil, ErrInvalidRequest
	}
	for _, term := range strings.Split(value, ",") {
		key, expected, ok := strings.Cut(strings.TrimSpace(term), "=")
		if !ok || !safeSelectorKey(key) || expected == "" || strings.ContainsAny(expected, "\r\n") {
			return nil, ErrInvalidRequest
		}
		result[key] = expected
	}
	return result, nil
}

func matchesSelectors(body json.RawMessage, labels, fields map[string]string) bool {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	metadata, _ := object["metadata"].(map[string]any)
	actualLabels, _ := metadata["labels"].(map[string]any)
	for key, expected := range labels {
		if fmt.Sprint(actualLabels[key]) != expected {
			return false
		}
	}
	for key, expected := range fields {
		var actual any
		switch key {
		case "metadata.name":
			actual = metadata["name"]
		case "metadata.namespace":
			actual = metadata["namespace"]
		case "type":
			actual = object["type"]
		case "spec.nodeName":
			spec, _ := object["spec"].(map[string]any)
			actual = spec["nodeName"]
		default:
			return false
		}
		if fmt.Sprint(actual) != expected {
			return false
		}
	}
	return true
}

func safeSegment(value string) bool {
	return value != "" && len(value) <= 253 && !strings.ContainsAny(value, "/\\\r\n")
}

func safeOptionalSegment(value string) bool {
	return value == "" || safeSegment(value)
}

func safeSelectorKey(value string) bool {
	return value != "" && len(value) <= 253 && !strings.ContainsAny(value, "=,!<> \t\r\n")
}
