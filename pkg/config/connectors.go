package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"gopkg.in/yaml.v3"
)

type ConnectorsConfig struct {
	Kubernetes KubernetesConnectorConfig `mapstructure:"kubernetes"`
}

type KubernetesConnectorConfig struct {
	Multiple             bool `mapstructure:"multiple"`
	KubernetesToolConfig `mapstructure:",squash"`
	Clusters             []KubernetesClusterConfig `mapstructure:"clusters"`
}

type KubernetesClusterConfig struct {
	DisplayName          string `mapstructure:"display_name"`
	KubernetesToolConfig `mapstructure:",squash"`
}

func (connector KubernetesConnectorConfig) Resolved() []KubernetesClusterConfig {
	if connector.Multiple {
		return append([]KubernetesClusterConfig(nil), connector.Clusters...)
	}
	return []KubernetesClusterConfig{{KubernetesToolConfig: connector.KubernetesToolConfig}}
}

type KubernetesToolConfig struct {
	Endpoint             string               `mapstructure:"endpoint"`
	TokenFile            string               `mapstructure:"token_file"`
	CAFile               string               `mapstructure:"ca_file"`
	CAData               string               `mapstructure:"ca_data"`
	ServerName           string               `mapstructure:"server_name"`
	Auth                 KubernetesAuthConfig `mapstructure:"auth"`
	ClusterID            string               `mapstructure:"cluster_id"`
	CredentialID         string               `mapstructure:"credential_id"`
	Timeout              string               `mapstructure:"timeout"`
	DiscoveryTTL         string               `mapstructure:"discovery_ttl"`
	ChangeCapture        string               `mapstructure:"change_capture"`
	AllowLoopback        bool                 `mapstructure:"allow_loopback"`
	AllowPrivateNetworks bool                 `mapstructure:"allow_private_networks"`
	EndpointCIDRs        []string             `mapstructure:"endpoint_cidrs"`
	// ExcludeResources names resources, by their plural API name such as
	// "secrets", that Versus never lists. A resource left out on purpose is
	// reported as not collected, never as a permission gap.
	ExcludeResources []string `mapstructure:"exclude_resources"`
	// Namespaces names the namespaces the connector reads when its identity
	// may read only inside some namespaces. They are read from the first sync
	// on, after every restart.
	Namespaces []string                    `mapstructure:"namespaces"`
	Actions    KubernetesActionsToolConfig `mapstructure:"actions"`
}

type KubernetesActionsToolConfig struct {
	Enable      bool                 `mapstructure:"enable"`
	Auth        KubernetesAuthConfig `mapstructure:"auth"`
	MaxReplicas int                  `mapstructure:"max_replicas"`
	Timeout     string               `mapstructure:"timeout"`
}

type KubernetesAuthConfig struct {
	Mode              string                            `mapstructure:"mode"`
	Token             string                            `mapstructure:"token"`
	TokenFile         string                            `mapstructure:"token_file"`
	ClientCertificate KubernetesClientCertificateConfig `mapstructure:"client_certificate"`
	Kubeconfig        KubernetesKubeconfigConfig        `mapstructure:"kubeconfig"`
	EKS               KubernetesEKSConfig               `mapstructure:"eks"`
	AKS               KubernetesAKSConfig               `mapstructure:"aks"`
	GKE               KubernetesGKEConfig               `mapstructure:"gke"`
}

type KubernetesClientCertificateConfig struct {
	CertificateFile string `mapstructure:"certificate_file"`
	KeyFile         string `mapstructure:"key_file"`
	CertificateData string `mapstructure:"certificate_data"`
	KeyData         string `mapstructure:"key_data"`
}

type KubernetesKubeconfigConfig struct {
	Path    string `mapstructure:"path"`
	Context string `mapstructure:"context"`
}

type KubernetesEKSConfig struct {
	ClusterName string `mapstructure:"cluster_name"`
	Region      string `mapstructure:"region"`
	RoleARN     string `mapstructure:"role_arn"`
	Profile     string `mapstructure:"profile"`
}

type KubernetesAKSConfig struct {
	CredentialMode     string `mapstructure:"credential_mode"`
	ServerID           string `mapstructure:"server_id"`
	TenantID           string `mapstructure:"tenant_id"`
	ClientID           string `mapstructure:"client_id"`
	ClientSecret       string `mapstructure:"client_secret"`
	FederatedTokenFile string `mapstructure:"federated_token_file"`
	Environment        string `mapstructure:"environment"`
}

type KubernetesGKEConfig struct {
	CredentialsFile string `mapstructure:"credentials_file"`
}

func decodeConnectors(value any) (ConnectorsConfig, error) {
	connectors := ConnectorsConfig{Kubernetes: KubernetesConnectorConfig{KubernetesToolConfig: KubernetesToolConfig{ChangeCapture: "continuous"}}}
	if value == nil {
		return connectors, nil
	}
	value = expandEnvironmentScalars(value)
	if err := validateMultipleShape(value); err != nil {
		return connectors, err
	}
	if err := normalizeConnectorCIDRs(value); err != nil {
		return connectors, err
	}
	if root, ok := value.(map[string]any); ok {
		for key, raw := range root {
			if !strings.EqualFold(key, "kubernetes") {
				continue
			}
			fields, _ := raw.(map[string]any)
			for name, rawEntries := range fields {
				if !strings.EqualFold(name, "clusters") {
					continue
				}
				entries, _ := rawEntries.([]any)
				for index, entry := range entries {
					if err := connectorShapeError(entry, reflect.TypeOf(KubernetesClusterConfig{}), fmt.Sprintf("connectors.kubernetes.clusters[%d]", index)); err != nil {
						return connectors, err
					}
				}
			}
		}
	}
	if err := connectorShapeError(value, reflect.TypeOf(connectors), "connectors"); err != nil {
		return connectors, err
	}
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result: &connectors, TagName: "mapstructure", ErrorUnused: true,
		DecodeHook: mapstructure.DecodeHookFuncType(connectorScalar),
	})
	if err != nil {
		return connectors, errors.New("invalid connectors configuration")
	}
	if err := decoder.Decode(value); err != nil {
		return ConnectorsConfig{}, errors.New("invalid connectors configuration")
	}
	if !connectors.Kubernetes.Multiple && connectors.Kubernetes.ChangeCapture == "" {
		connectors.Kubernetes.ChangeCapture = "continuous"
	}
	for position := range connectors.Kubernetes.Clusters {
		if connectors.Kubernetes.Clusters[position].ChangeCapture == "" {
			connectors.Kubernetes.Clusters[position].ChangeCapture = "continuous"
		}
	}
	if err := validateKubernetesClusters(connectors.Kubernetes); err != nil {
		return ConnectorsConfig{}, err
	}
	return connectors, nil
}

func validateMultipleShape(value any) error {
	root, _ := value.(map[string]any)
	for key, nested := range root {
		if !strings.EqualFold(key, "kubernetes") {
			continue
		}
		fields, _ := nested.(map[string]any)
		multiple := false
		for name, raw := range fields {
			if strings.EqualFold(name, "multiple") {
				switch typed := raw.(type) {
				case bool:
					multiple = typed
				case string:
					multiple, _ = strconv.ParseBool(typed)
				}
			}
		}
		for name := range fields {
			if strings.EqualFold(name, "clusters") && !multiple {
				return errors.New("connectors.kubernetes.clusters requires multiple: true")
			}
			if multiple && !strings.EqualFold(name, "multiple") && !strings.EqualFold(name, "clusters") {
				return errors.New("connectors.kubernetes: configure each cluster under clusters")
			}
		}
	}
	return nil
}

// kubernetesResourceName is the shape of a plural API resource name.
var kubernetesResourceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// kubernetesNamespaceName is a DNS-1123 label, the only shape a namespace has.
var kubernetesNamespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateNameList checks a bounded list of distinct names of one shape.
func validateNameList(key string, values []string, shape *regexp.Regexp) error {
	if len(values) > 32 {
		return fmt.Errorf("connectors.kubernetes.%s lists more than 32 entries", key)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !shape.MatchString(value) {
			return fmt.Errorf("connectors.kubernetes.%s: %q is not a valid name", key, value)
		}
		if seen[value] {
			return fmt.Errorf("connectors.kubernetes.%s lists %q twice", key, value)
		}
		seen[value] = true
	}
	return nil
}

func validateKubernetesClusters(connector KubernetesConnectorConfig) error {
	for _, entry := range connector.Resolved() {
		if entry.ChangeCapture != "" && entry.ChangeCapture != "continuous" && entry.ChangeCapture != "demand" {
			return errors.New("connectors.kubernetes.change_capture must be continuous or demand")
		}
		if err := validateNameList("exclude_resources", entry.ExcludeResources, kubernetesResourceName); err != nil {
			return err
		}
		for _, resource := range entry.ExcludeResources {
			if resource == "pods" || resource == "namespaces" {
				return fmt.Errorf("connectors.kubernetes.exclude_resources cannot exclude %q: every Kubernetes view is built from it", resource)
			}
		}
		if err := validateNameList("namespaces", entry.Namespaces, kubernetesNamespaceName); err != nil {
			return err
		}
	}
	if !connector.Multiple {
		return nil
	}
	if len(connector.Clusters) == 0 || len(connector.Clusters) > 16 {
		return errors.New("connectors.kubernetes.clusters must contain 1 to 16 entries")
	}
	ids, endpoints := map[string]bool{}, map[string]bool{}
	inCluster := false
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	for index, entry := range connector.Clusters {
		fail := func() error {
			return fmt.Errorf("connectors.kubernetes.clusters[%d]: invalid or duplicate cluster identity or endpoint", index)
		}
		if !label.MatchString(entry.ClusterID) || ids[entry.ClusterID] {
			return fail()
		}
		ids[entry.ClusterID] = true
		endpoint := strings.TrimRight(strings.TrimSpace(entry.Endpoint), "/")
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			parsed.Host = strings.ToLower(parsed.Host)
			if parsed.Scheme == "https" {
				parsed.Host = strings.TrimSuffix(parsed.Host, ":443")
			}
			endpoint = parsed.String()
		}
		if endpoint != "" {
			if endpoints[endpoint] {
				return fail()
			}
			endpoints[endpoint] = true
		}
		if entry.Auth.Mode == "in_cluster" {
			if inCluster {
				return fail()
			}
			inCluster = true
		}
		if len(entry.DisplayName) > 128 {
			return fmt.Errorf("connectors.kubernetes.clusters[%d]: display_name exceeds 128 bytes", index)
		}
	}
	return nil
}

func normalizeConnectorCIDRs(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, nested := range root {
		if !strings.EqualFold(key, "kubernetes") {
			continue
		}
		fields, ok := nested.(map[string]any)
		if !ok {
			continue
		}
		if err := normalizeKubernetesCIDRs(fields); err != nil {
			return err
		}
	}
	return nil
}

func normalizeKubernetesCIDRs(fields map[string]any) error {
	for name, raw := range fields {
		if strings.EqualFold(name, "clusters") {
			entries, _ := raw.([]any)
			for index, entry := range entries {
				if child, ok := entry.(map[string]any); ok {
					if err := normalizeKubernetesCIDRs(child); err != nil {
						return fmt.Errorf("connectors.kubernetes.clusters[%d]: invalid endpoint CIDRs", index)
					}
				}
			}
		}
		if !strings.EqualFold(name, "endpoint_cidrs") {
			continue
		}
		var items []any
		switch typed := raw.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				for _, item := range strings.Split(typed, ",") {
					items = append(items, item)
				}
			}
		case []string:
			for _, item := range typed {
				items = append(items, item)
			}
		case []any:
			items = typed
		default:
			return errors.New("invalid connectors endpoint CIDRs")
		}
		cidrs := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return errors.New("invalid connectors endpoint CIDRs")
			}
			text = strings.TrimSpace(text)
			if _, err := netip.ParsePrefix(text); err != nil {
				return errors.New("invalid connectors endpoint CIDRs")
			}
			cidrs = append(cidrs, text)
		}
		fields[name] = cidrs
	}
	return nil
}

func connectorScalar(source, target reflect.Type, value any) (any, error) {
	if source.Kind() != reflect.String {
		return value, nil
	}
	switch target.Kind() {
	case reflect.Bool:
		return strconv.ParseBool(value.(string))
	case reflect.Int:
		return strconv.Atoi(value.(string))
	default:
		return value, nil
	}
}

// connectorShapeError checks a decoded connector block against its Go type
// and names the first problem by YAML path: an unknown key by its name, a
// wrong type as an invalid value. Values are never included, because a
// connector value may be a credential.
func connectorShapeError(value any, target reflect.Type, path string) error {
	if value == nil {
		return fmt.Errorf("%s: invalid value", path)
	}
	if target.Kind() != reflect.Struct {
		if reflect.TypeOf(value).AssignableTo(target) {
			return nil
		}
		if reflect.TypeOf(value).Kind() == reflect.String && (target.Kind() == reflect.Bool || target.Kind() == reflect.Int) {
			if _, err := connectorScalar(reflect.TypeOf(value), target, value); err == nil {
				return nil
			}
			return fmt.Errorf("%s: invalid value", path)
		}
		if target.Kind() == reflect.Slice {
			return connectorSliceError(value, target, path)
		}
		return fmt.Errorf("%s: invalid value", path)
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: invalid value", path)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fieldType, found := connectorFieldType(target, key)
		if !found {
			return fmt.Errorf("%s: unknown key %q", path, connectorKeyForError(key))
		}
		if err := connectorShapeError(fields[key], fieldType, path+"."+connectorKeyForError(key)); err != nil {
			return err
		}
	}
	return nil
}

// connectorKeyForError bounds a key before it is echoed in an error, so a
// value pasted where a key belongs cannot be reproduced in full.
func connectorKeyForError(key string) string {
	if runes := []rune(key); len(runes) > 64 {
		return string(runes[:64]) + "..."
	}
	return key
}

func connectorFieldType(target reflect.Type, key string) (reflect.Type, bool) {
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.Tag.Get("mapstructure") == ",squash" {
			if nested, found := connectorFieldType(field.Type, key); found {
				return nested, true
			}
		} else if strings.EqualFold(key, field.Tag.Get("mapstructure")) {
			return field.Type, true
		}
	}
	return nil, false
}

func connectorSliceError(value any, target reflect.Type, path string) error {
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s: invalid value", path)
	}
	for index, item := range items {
		if err := connectorShapeError(item, target.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	return nil
}

func rawConfigSettings(raw []byte) (map[string]any, error) {
	var settings map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	err := decoder.Decode(&settings)
	if errors.Is(err, io.EOF) {
		return map[string]any{}, nil
	}
	if err != nil || settings == nil {
		return nil, errors.New("invalid configuration YAML")
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, errors.New("invalid configuration YAML")
	}
	return settings, nil
}

func configValue(settings map[string]any, path ...string) (any, bool) {
	for key, value := range settings {
		segments := strings.Split(key, ".")
		if len(segments) <= len(path) && strings.EqualFold(key, strings.Join(path[:len(segments)], ".")) {
			if len(segments) == len(path) {
				return value, true
			}
			if nested, ok := value.(map[string]any); ok {
				if found, present := configValue(nested, path[len(segments):]...); present {
					return found, true
				}
			}
		}
	}
	return nil, false
}

func rejectToolConnectors(raw []byte) error {
	settings, err := rawConfigSettings(raw)
	if err != nil {
		return err
	}
	for _, path := range [][]string{{"tools", "kubernetes"}, {"agent", "tools", "kubernetes"}} {
		if _, present := configValue(settings, path...); present {
			return errors.New("Kubernetes configuration must use connectors.kubernetes")
		}
	}
	return nil
}

// loadSiblingConnectorsFile reads the connectors.yaml next to the main
// config. present is false when the file holds no YAML document at all
// (empty or comments only), so the caller keeps the inline connectors.
func loadSiblingConnectorsFile(path string) (config ConnectorsConfig, present bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ConnectorsConfig{}, false, errors.New("cannot read connectors file")
	}
	settings, err := rawConfigSettings(raw)
	if err != nil {
		return ConnectorsConfig{}, false, errors.New("invalid connectors YAML")
	}
	if len(settings) == 0 {
		return ConnectorsConfig{}, false, nil
	}
	config, err = loadConnectorsFile(path)
	return config, err == nil, err
}

func loadConnectorsFile(path string) (ConnectorsConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ConnectorsConfig{}, errors.New("cannot read connectors file")
	}
	settings, err := rawConfigSettings(raw)
	if err != nil {
		return ConnectorsConfig{}, errors.New("invalid connectors YAML")
	}
	for key := range settings {
		if !strings.EqualFold(key, "connectors") {
			return ConnectorsConfig{}, errors.New("invalid connectors file key")
		}
	}
	value, present := configValue(settings, "connectors")
	if !present || value == nil {
		return ConnectorsConfig{}, errors.New("invalid connectors configuration")
	}
	return decodeConnectors(value)
}
