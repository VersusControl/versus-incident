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
	Endpoint             string                      `mapstructure:"endpoint"`
	TokenFile            string                      `mapstructure:"token_file"`
	CAFile               string                      `mapstructure:"ca_file"`
	CAData               string                      `mapstructure:"ca_data"`
	ServerName           string                      `mapstructure:"server_name"`
	Auth                 KubernetesAuthConfig        `mapstructure:"auth"`
	ClusterID            string                      `mapstructure:"cluster_id"`
	CredentialID         string                      `mapstructure:"credential_id"`
	Timeout              string                      `mapstructure:"timeout"`
	DiscoveryTTL         string                      `mapstructure:"discovery_ttl"`
	AllowLoopback        bool                        `mapstructure:"allow_loopback"`
	AllowPrivateNetworks bool                        `mapstructure:"allow_private_networks"`
	EndpointCIDRs        []string                    `mapstructure:"endpoint_cidrs"`
	Actions              KubernetesActionsToolConfig `mapstructure:"actions"`
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
	var connectors ConnectorsConfig
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
					if !validConnectorShape(entry, reflect.TypeOf(KubernetesClusterConfig{})) {
						return connectors, fmt.Errorf("connectors.kubernetes.clusters[%d]: invalid configuration", index)
					}
				}
			}
		}
	}
	if !validConnectorShape(value, reflect.TypeOf(connectors)) {
		return connectors, errors.New("invalid connectors configuration")
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

func validateKubernetesClusters(connector KubernetesConnectorConfig) error {
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

func validConnectorShape(value any, target reflect.Type) bool {
	if value == nil {
		return false
	}
	if target.Kind() != reflect.Struct {
		if reflect.TypeOf(value).AssignableTo(target) {
			return true
		}
		if reflect.TypeOf(value).Kind() == reflect.String && (target.Kind() == reflect.Bool || target.Kind() == reflect.Int) {
			_, err := connectorScalar(reflect.TypeOf(value), target, value)
			return err == nil
		}
		return target.Kind() == reflect.Slice && validConnectorSlice(value, target)
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, nested := range fields {
		fieldType, found := connectorFieldType(target, key)
		if !found || !validConnectorShape(nested, fieldType) {
			return false
		}
	}
	return true
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

func validConnectorSlice(value any, target reflect.Type) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if !validConnectorShape(item, target.Elem()) {
			return false
		}
	}
	return true
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
