package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	DefaultLimit                 = 20
	MaximumLimit                 = 50
	MaximumFields                = 32
	MaximumMappingFieldsPerIndex = 50
	searchTotalThreshold         = 10000
	searchTerminateAfter         = 10000
)

// Service provides provider-neutral, bounded reads for one configured source.
type Service struct {
	client        *Client
	configured    config.AgentElasticsearchSourceConfig
	allowedFields map[string]struct{}
	scrubber      core.Scrubber
}

type Index struct {
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
	Documents uint64 `json:"documents,omitempty"`
}

type MappingField struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

type IndexMapping struct {
	Index       string         `json:"index"`
	Fields      []MappingField `json:"fields"`
	TotalFields int            `json:"total_fields"`
	Truncated   bool           `json:"truncated"`
}

type Shard struct {
	Index     string  `json:"index"`
	Shard     int     `json:"shard"`
	Primary   bool    `json:"primary"`
	State     string  `json:"state"`
	Documents *uint64 `json:"documents,omitempty"`
	Store     string  `json:"store,omitempty"`
	Node      string  `json:"node,omitempty"`
}

type SearchOptions struct {
	QueryBody json.RawMessage
	Fields    []string
	Limit     int
}

type SearchHit struct {
	ID     string         `json:"id,omitempty"`
	Fields map[string]any `json:"fields"`
}

type SearchResult struct {
	Total     uint64      `json:"total"`
	Relation  string      `json:"relation"`
	TimedOut  bool        `json:"timed_out"`
	Truncated bool        `json:"truncated"`
	Hits      []SearchHit `json:"hits"`
}

type IndexResult struct {
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
	Items     []Index `json:"items"`
}

type MappingResult struct {
	TotalIndices           int            `json:"total_indices"`
	Truncated              bool           `json:"truncated"`
	AllowedFields          []string       `json:"allowed_fields"`
	AllowedFieldsTruncated bool           `json:"allowed_fields_truncated"`
	Items                  []IndexMapping `json:"items"`
}

type ShardResult struct {
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
	Items     []Shard `json:"items"`
}

// NewService constructs one source-scoped application service.
func NewService(cfg config.AgentElasticsearchSourceConfig) (*Service, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.TimeField == "" {
		cfg.TimeField = "@timestamp"
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "message"
	}
	if cfg.TieBreakerField == "" {
		cfg.TieBreakerField = "event.id"
	}
	if err := normalizeConfiguredFields(&cfg); err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{})
	for _, field := range projectedFields(cfg) {
		allowed[field] = struct{}{}
	}
	return &Service{client: client, configured: cfg, allowedFields: allowed}, nil
}

// Client returns the shared source-scoped transport used by signal tailing.
func (service *Service) Client() *Client {
	if service == nil {
		return nil
	}
	return service.client
}

// Config returns the normalized source configuration used by this service.
func (service *Service) Config() config.AgentElasticsearchSourceConfig {
	if service == nil {
		return config.AgentElasticsearchSourceConfig{}
	}
	configured := service.configured
	configured.Addresses = append([]string(nil), configured.Addresses...)
	configured.ExtraFields = append([]string(nil), configured.ExtraFields...)
	return configured
}

// ProjectedFields returns the validated fields this source may retrieve.
func (service *Service) ProjectedFields() []string {
	if service == nil {
		return nil
	}
	return projectedFields(service.configured)
}

// SetScrubber applies the shared redaction policy to every backend string
// before a result can cross the application-service boundary.
func (service *Service) SetScrubber(scrubber core.Scrubber) {
	if service != nil {
		service.scrubber = scrubber
	}
}

// ListIndices discovers indices matching only the configured pattern.
func (service *Service) ListIndices(ctx context.Context, limit int) (IndexResult, error) {
	limit = boundedLimit(limit)
	query := url.Values{"format": {"json"}, "h": {"index,status,docs.count"}}
	var raw []struct {
		Index  string `json:"index"`
		Status string `json:"status"`
		Docs   string `json:"docs.count"`
	}
	if err := service.client.readJSON(ctx, service.client.toolPolicy, http.MethodGet, "/_cat/indices", query, nil, &raw); err != nil {
		return IndexResult{}, err
	}
	result := make([]Index, 0, min(limit, len(raw)))
	for _, item := range raw {
		if len(result) == limit {
			break
		}
		documents, _ := strconv.ParseUint(item.Docs, 10, 64)
		result = append(result, Index{Name: service.scrub(item.Index, 256), Status: service.scrub(item.Status, 32), Documents: documents})
	}
	return IndexResult{Total: len(raw), Truncated: len(raw) > len(result), Items: result}, nil
}

// Mappings returns a flattened, bounded field map for matching indices.
func (service *Service) Mappings(ctx context.Context, limit int) (MappingResult, error) {
	limit = boundedLimit(limit)
	query := service.mappingQuery()
	var raw map[string]struct {
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := service.client.readJSON(ctx, service.client.toolPolicy, http.MethodGet, "/_mapping", query, nil, &raw); err != nil {
		return MappingResult{}, err
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]IndexMapping, 0, min(len(names), limit))
	for _, name := range names {
		if len(result) == limit {
			break
		}
		allFields := service.allowedMappingFields(raw[name].Mappings.Properties)
		totalFields := len(allFields)
		fieldsTruncated := totalFields > MaximumMappingFieldsPerIndex
		if fieldsTruncated {
			allFields = allFields[:MaximumMappingFieldsPerIndex]
		}
		for index := range allFields {
			allFields[index].Name = service.scrub(allFields[index].Name, 512)
			allFields[index].Type = service.scrub(allFields[index].Type, 64)
		}
		result = append(result, IndexMapping{Index: service.scrub(name, 256), Fields: allFields, TotalFields: totalFields, Truncated: fieldsTruncated})
	}
	allowedFields, allowedFieldsTruncated := service.boundedAllowedFields()
	return MappingResult{TotalIndices: len(names), Truncated: len(names) > len(result), AllowedFields: allowedFields, AllowedFieldsTruncated: allowedFieldsTruncated, Items: result}, nil
}

func (service *Service) boundedAllowedFields() ([]string, bool) {
	fields := make([]string, 0, len(service.allowedFields))
	for field := range service.allowedFields {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	truncated := len(fields) > MaximumFields
	if truncated {
		fields = fields[:MaximumFields]
	}
	return fields, truncated
}

func (service *Service) mappingQuery() url.Values {
	fields := make([]string, 0, len(service.allowedFields))
	for field := range service.allowedFields {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	paths := make([]string, 0, len(fields))
	for _, field := range fields {
		parts := strings.Split(field, ".")
		for _, part := range parts {
			if part == "" {
				return url.Values{"filter_path": {"*.mappings.properties"}}
			}
		}
		paths = append(paths, "*.mappings.properties."+strings.Join(parts, ".properties."))
	}
	query := url.Values{"filter_path": {strings.Join(paths, ",")}}
	if len(query.Encode()) > maximumRequestBytes {
		return url.Values{"filter_path": {"*.mappings.properties"}}
	}
	return query
}

// Shards returns bounded health fields for shards in the configured pattern.
func (service *Service) Shards(ctx context.Context, limit int) (ShardResult, error) {
	limit = boundedLimit(limit)
	query := url.Values{"format": {"json"}, "h": {"index,shard,prirep,state,docs,store,node"}}
	var raw []struct {
		Index  string `json:"index"`
		Shard  string `json:"shard"`
		PriRep string `json:"prirep"`
		State  string `json:"state"`
		Docs   string `json:"docs"`
		Store  string `json:"store"`
		Node   string `json:"node"`
	}
	if err := service.client.readJSON(ctx, service.client.toolPolicy, http.MethodGet, "/_cat/shards", query, nil, &raw); err != nil {
		return ShardResult{}, err
	}
	result := make([]Shard, 0, min(limit, len(raw)))
	for _, item := range raw {
		if len(result) == limit {
			break
		}
		shard, _ := strconv.Atoi(item.Shard)
		var documents *uint64
		if value, err := strconv.ParseUint(item.Docs, 10, 64); err == nil {
			documents = &value
		}
		result = append(result, Shard{Index: service.scrub(item.Index, 256), Shard: shard, Primary: item.PriRep == "p", State: service.scrub(item.State, 32), Documents: documents, Store: service.scrub(item.Store, 64), Node: service.scrub(item.Node, 256)})
	}
	return ShardResult{Total: len(raw), Truncated: len(raw) > len(result), Items: result}, nil
}

// Search executes sanitized Query DSL and projects only configured source fields.
func (service *Service) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	limit := boundedLimit(options.Limit)
	body, fields, projectionTruncated, err := service.searchBody(options, limit)
	if err != nil {
		return SearchResult{}, err
	}
	var raw struct {
		TimedOut        bool `json:"timed_out"`
		TerminatedEarly bool `json:"terminated_early"`
		Hits            struct {
			Total json.RawMessage `json:"total"`
			Hits  []struct {
				ID     string         `json:"_id"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := service.client.SearchJSON(ctx, body, &raw); err != nil {
		return SearchResult{}, err
	}
	total, relation := decodeTotal(raw.Hits.Total)
	result := SearchResult{Total: total, Relation: relation, TimedOut: raw.TimedOut, Hits: make([]SearchHit, 0, min(limit, len(raw.Hits.Hits)))}
	for _, item := range raw.Hits.Hits {
		if len(result.Hits) == limit {
			break
		}
		projected := make(map[string]any, len(fields))
		for _, field := range fields {
			if value, ok := lookup(item.Source, field); ok {
				projected[field] = service.scrubValue(value)
			}
		}
		result.Hits = append(result.Hits, SearchHit{ID: service.scrub(item.ID, 256), Fields: projected})
	}
	result.Truncated = projectionTruncated || raw.TimedOut || raw.TerminatedEarly || relation != "eq" || total > uint64(len(result.Hits)) || len(raw.Hits.Hits) > len(result.Hits)
	return result, nil
}

func (service *Service) searchBody(options SearchOptions, limit int) ([]byte, []string, bool, error) {
	if len(options.QueryBody) > maximumRequestBytes {
		return nil, nil, false, ErrInvalidArguments
	}
	root := make(map[string]any)
	if len(options.QueryBody) > 0 && string(options.QueryBody) != "null" {
		if err := json.Unmarshal(options.QueryBody, &root); err != nil {
			return nil, nil, false, ErrInvalidArguments
		}
	}
	for key := range root {
		if key != "query" && key != "sort" && key != "_source" {
			return nil, nil, false, ErrInvalidArguments
		}
	}
	if !service.safeDSL(root) {
		return nil, nil, false, ErrInvalidArguments
	}
	fields, projectionTruncated, err := service.resolveFields(options.Fields, root["_source"])
	if err != nil {
		return nil, nil, false, err
	}
	delete(root, "_source")
	if configured := strings.TrimSpace(service.configured.Query); configured != "" {
		configuredQuery := map[string]any{"query_string": map[string]any{"query": configured}}
		if requested, ok := root["query"]; ok {
			root["query"] = map[string]any{"bool": map[string]any{"must": []any{configuredQuery, requested}}}
		} else {
			root["query"] = configuredQuery
		}
	}
	root["size"] = limit
	root["track_total_hits"] = searchTotalThreshold
	root["timeout"] = "5s"
	root["terminate_after"] = searchTerminateAfter
	root["_source"] = fields
	body, err := json.Marshal(root)
	if err != nil || len(body) > maximumRequestBytes {
		return nil, nil, false, ErrInvalidArguments
	}
	return body, fields, projectionTruncated, nil
}

func (service *Service) resolveFields(requested []string, source any) ([]string, bool, error) {
	fields := append([]string(nil), requested...)
	if source != nil {
		values, ok := source.([]any)
		if !ok {
			return nil, false, ErrInvalidArguments
		}
		for _, value := range values {
			field, ok := value.(string)
			if !ok {
				return nil, false, ErrInvalidArguments
			}
			fields = append(fields, field)
		}
	}
	truncated := false
	if len(fields) == 0 {
		for field := range service.allowedFields {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		if len(fields) > MaximumFields {
			truncated = true
			fields = fields[:MaximumFields]
		}
	} else if len(fields) > MaximumFields {
		return nil, false, ErrInvalidArguments
	}
	seen := make(map[string]struct{}, len(fields))
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, allowed := service.allowedFields[field]; !allowed {
			return nil, false, ErrInvalidArguments
		}
		if _, duplicate := seen[field]; !duplicate {
			seen[field] = struct{}{}
			result = append(result, field)
		}
	}
	sort.Strings(result)
	return result, truncated, nil
}

func (service *Service) safeDSL(root map[string]any) bool {
	if sortValue, ok := root["sort"]; ok && !service.safeSort(sortValue) {
		return false
	}
	query, ok := root["query"]
	return !ok || service.safeQuery(query, 0)
}

func (service *Service) safeQuery(value any, depth int) bool {
	if depth > 20 {
		return false
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return false
	}
	if len(object) != 1 {
		return false
	}
	for operator, child := range object {
		switch strings.ToLower(operator) {
		case "bool":
			clauses, ok := child.(map[string]any)
			if !ok || len(clauses) == 0 {
				return false
			}
			for key, clause := range clauses {
				switch key {
				case "must", "filter", "should", "must_not":
					if !service.safeQueryClause(clause, depth+1) {
						return false
					}
				default:
					return false
				}
			}
		case "term":
			fields, ok := child.(map[string]any)
			if !ok || len(fields) != 1 {
				return false
			}
			for field, fieldValue := range fields {
				if !service.allowedField(field) || !safeScalar(fieldValue) {
					return false
				}
			}
		case "terms":
			fields, ok := child.(map[string]any)
			if !ok || len(fields) != 1 {
				return false
			}
			for field, fieldValue := range fields {
				values, valuesOK := fieldValue.([]any)
				if !service.allowedField(field) || !valuesOK || len(values) == 0 || len(values) > 1000 {
					return false
				}
				for _, item := range values {
					if !safeScalar(item) {
						return false
					}
				}
			}
		case "range":
			fields, ok := child.(map[string]any)
			if !ok || len(fields) != 1 {
				return false
			}
			for field, fieldValue := range fields {
				parameters, parametersOK := fieldValue.(map[string]any)
				if !service.allowedField(field) || !parametersOK || len(parameters) == 0 {
					return false
				}
				for name, parameter := range parameters {
					if (name != "gt" && name != "gte" && name != "lt" && name != "lte") || !safeScalar(parameter) {
						return false
					}
				}
			}
		case "match_phrase":
			fields, ok := child.(map[string]any)
			if !ok || len(fields) != 1 {
				return false
			}
			for field, fieldValue := range fields {
				text, textOK := fieldValue.(string)
				if !service.allowedField(field) || !textOK || !boundedDSLString(text) {
					return false
				}
			}
		case "match":
			fields, ok := child.(map[string]any)
			if !ok || len(fields) != 1 {
				return false
			}
			for field, fieldValue := range fields {
				text, textOK := fieldValue.(string)
				if !service.allowedField(field) || !textOK || !boundedDSLString(text) {
					return false
				}
			}
		case "match_all":
			if parameters, ok := child.(map[string]any); !ok || len(parameters) != 0 {
				return false
			}
		case "more_like_this", "regexp", "wildcard", "fuzzy", "nested", "has_child", "has_parent", "percolate", "script", "query_string", "simple_query_string", "multi_match", "runtime_mappings", "lookup", "pit", "indices":
			return false
		default:
			return false
		}
	}
	return true
}

func safeScalar(value any) bool {
	switch typed := value.(type) {
	case string:
		return boundedDSLString(typed)
	case float64, bool:
		return true
	default:
		return false
	}
}

func boundedDSLString(value string) bool {
	return utf8.RuneCountInString(value) <= 4096
}

func (service *Service) safeQueryClause(value any, depth int) bool {
	if values, ok := value.([]any); ok {
		if len(values) == 0 || len(values) > 1000 {
			return false
		}
		for _, item := range values {
			if !service.safeQuery(item, depth) {
				return false
			}
		}
		return true
	}
	return service.safeQuery(value, depth)
}

func (service *Service) safeSort(value any) bool {
	values, ok := value.([]any)
	if !ok || len(values) == 0 || len(values) > MaximumFields {
		return false
	}
	for _, item := range values {
		switch typed := item.(type) {
		case string:
			if !service.allowedField(typed) {
				return false
			}
		case map[string]any:
			if len(typed) != 1 {
				return false
			}
			for field, options := range typed {
				if !service.allowedField(field) {
					return false
				}
				switch sortOptions := options.(type) {
				case string:
					if sortOptions != "asc" && sortOptions != "desc" {
						return false
					}
				case map[string]any:
					if len(sortOptions) != 1 || (sortOptions["order"] != "asc" && sortOptions["order"] != "desc") {
						return false
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

func (service *Service) allowedMappingFields(properties map[string]any) []MappingField {
	fields := make([]MappingField, 0, len(service.allowedFields))
	for field := range service.allowedFields {
		if definition, ok := mappingDefinition(properties, field); ok {
			kind, _ := definition["type"].(string)
			fields = append(fields, MappingField{Name: field, Type: kind})
		}
	}
	sort.Slice(fields, func(left, right int) bool { return fields[left].Name < fields[right].Name })
	return fields
}

func mappingDefinition(properties map[string]any, field string) (map[string]any, bool) {
	current := properties
	parts := strings.Split(field, ".")
	for index, part := range parts {
		raw, ok := current[part]
		definition, definitionOK := raw.(map[string]any)
		if !ok || !definitionOK {
			return nil, false
		}
		if index == len(parts)-1 {
			return definition, true
		}
		current, ok = definition["properties"].(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

func (service *Service) allowedField(field string) bool {
	if field == "_index" || field == "_id" {
		return false
	}
	_, ok := service.allowedFields[field]
	return ok
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return min(limit, MaximumLimit)
}

func validField(field string) bool {
	field = strings.TrimSpace(field)
	if field == "" || len(field) > 512 || strings.HasPrefix(field, "_") {
		return false
	}
	for _, part := range strings.Split(field, ".") {
		if part == "" {
			return false
		}
		for _, character := range part {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '@' {
				continue
			}
			return false
		}
	}
	return true
}

func normalizeConfiguredFields(cfg *config.AgentElasticsearchSourceConfig) error {
	type namedField struct {
		name     string
		value    *string
		optional bool
	}
	fields := []namedField{
		{name: "time_field", value: &cfg.TimeField},
		{name: "tie_breaker_field", value: &cfg.TieBreakerField},
		{name: "message_field", value: &cfg.MessageField},
		{name: "severity_field", value: &cfg.SeverityField, optional: true},
	}
	for _, field := range fields {
		*field.value = strings.TrimSpace(*field.value)
		if field.optional && *field.value == "" {
			continue
		}
		if !validField(*field.value) {
			return fmt.Errorf("%w: %s must be a concrete Elasticsearch field name without wildcards or metadata-field prefixes", ErrInvalidConfig, field.name)
		}
	}
	for index := range cfg.ExtraFields {
		cfg.ExtraFields[index] = strings.TrimSpace(cfg.ExtraFields[index])
		if !validField(cfg.ExtraFields[index]) {
			return fmt.Errorf("%w: extra_fields[%d] must be a concrete Elasticsearch field name without wildcards or metadata-field prefixes", ErrInvalidConfig, index)
		}
	}
	if cfg.TieBreakerField == cfg.TimeField {
		return fmt.Errorf("%w: tie_breaker_field must differ from time_field and identify a unique doc-values field", ErrInvalidConfig)
	}
	return nil
}

func projectedFields(cfg config.AgentElasticsearchSourceConfig) []string {
	candidates := append([]string{cfg.TimeField, cfg.TieBreakerField, cfg.MessageField, cfg.SeverityField}, cfg.ExtraFields...)
	seen := make(map[string]struct{}, len(candidates))
	fields := make([]string, 0, len(candidates))
	for _, field := range candidates {
		if field == "" {
			continue
		}
		if _, duplicate := seen[field]; duplicate {
			continue
		}
		seen[field] = struct{}{}
		fields = append(fields, field)
	}
	return fields
}

func boundedString(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func lookup(source map[string]any, field string) (any, bool) {
	if value, ok := source[field]; ok {
		return value, true
	}
	var current any = source
	for _, part := range strings.Split(field, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func decodeTotal(raw json.RawMessage) (uint64, string) {
	var direct uint64
	if json.Unmarshal(raw, &direct) == nil {
		return direct, "eq"
	}
	var object struct {
		Value    uint64 `json:"value"`
		Relation string `json:"relation"`
	}
	_ = json.Unmarshal(raw, &object)
	switch object.Relation {
	case "eq", "gte":
		return object.Value, object.Relation
	default:
		return object.Value, "gte"
	}
}

func (service *Service) scrub(value string, limit int) string {
	if service.scrubber != nil {
		value = service.scrubber.Scrub(value)
	}
	return boundedString(value, limit)
}

func (service *Service) scrubValue(value any) any {
	switch typed := value.(type) {
	case string:
		return service.scrub(typed, 4096)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = service.scrubValue(typed[index])
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = service.scrubValue(child)
		}
		return result
	default:
		return value
	}
}
