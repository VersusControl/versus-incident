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
	"time"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/go-viper/mapstructure/v2"
	"gopkg.in/yaml.v3"
)

type ConnectorsConfig struct {
	Kubernetes KubernetesConnectorConfig `mapstructure:"kubernetes"`
	AWS        AWSConnectorConfig        `mapstructure:"aws"`
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

// AWSConnectorConfig describes the AWS accounts Versus may read. Accounts in
// this file are read-only in the UI; wizard-created accounts live in storage.
type AWSConnectorConfig struct {
	Enable              bool                  `mapstructure:"enable"`
	Accounts            []AWSAccountConfig    `mapstructure:"accounts"`
	Tags                AWSTagsConfig         `mapstructure:"tags"`
	Budgets             AWSBudgetsConfig      `mapstructure:"budgets"`
	OptionalData        AWSOptionalDataConfig `mapstructure:"optional_data"`
	HealthCheckInterval string                `mapstructure:"health_check_interval"`
	Collect             AWSCollectConfig      `mapstructure:"collect"`
	Applications        AWSApplicationsConfig `mapstructure:"applications"`
	FinOps              AWSFinOpsConfig       `mapstructure:"finops"`
}

// AWSCollectConfig controls the background collectors that keep the
// committed AWS snapshots current. Collection is on by default whenever the
// connector is enabled.
type AWSCollectConfig struct {
	Enable    bool                      `mapstructure:"enable"`
	Intervals AWSCollectIntervalsConfig `mapstructure:"intervals"`
	History   AWSCollectHistoryConfig   `mapstructure:"history"`
}

// AWSCollectIntervalsConfig is how often each kind of unit is re-read.
type AWSCollectIntervalsConfig struct {
	Inventory string `mapstructure:"inventory"`
	Health    string `mapstructure:"health"`
	Changes   string `mapstructure:"changes"`
}

// AWSCollectHistoryConfig bounds the snapshot history the file store keeps.
// Postgres keeps every generation.
type AWSCollectHistoryConfig struct {
	MaxGenerations  int    `mapstructure:"max_generations"`
	MaxAge          string `mapstructure:"max_age"`
	ChangeRetention string `mapstructure:"change_retention"`
}

// AWSApplicationsConfig is the application resolver: the source order and
// the tag keys the tag source reads.
type AWSApplicationsConfig struct {
	Sources []string `mapstructure:"sources"`
	TagKeys []string `mapstructure:"tag_keys"`
}

// Collector defaults, used when a value is not set.
const (
	DefaultAWSInventoryInterval  = 15 * time.Minute
	DefaultAWSHealthInterval     = 2 * time.Minute
	DefaultAWSChangesInterval    = 5 * time.Minute
	DefaultAWSHistoryGenerations = 48
	DefaultAWSHistoryMaxAge      = 7 * 24 * time.Hour
	DefaultAWSChangeRetention    = 14 * 24 * time.Hour
	minAWSCollectInterval        = 30 * time.Second
	maxAWSCollectInterval        = 24 * time.Hour
	minAWSHistoryAge             = time.Hour
	maxAWSHistoryAge             = 366 * 24 * time.Hour
	maxAWSHistoryGenerations     = 10000
	maxAWSApplicationTagKeys     = 16
	awsApplicationSourceAppReg   = "appregistry"
	awsApplicationSourceStack    = "cloudformation"
	awsApplicationSourceTags     = "tags"
	awsCollectEnvironmentPrefix  = "VERSUS_CONNECTORS_AWS_COLLECT_"
	awsCollectEnableEnvironment  = awsCollectEnvironmentPrefix + "ENABLE"
	awsCollectInventoryEnv       = awsCollectEnvironmentPrefix + "INVENTORY_INTERVAL"
	awsCollectHealthEnv          = awsCollectEnvironmentPrefix + "HEALTH_INTERVAL"
	awsCollectChangesEnv         = awsCollectEnvironmentPrefix + "CHANGES_INTERVAL"
	awsCollectGenerationsEnv     = awsCollectEnvironmentPrefix + "HISTORY_MAX_GENERATIONS"
	awsCollectMaxAgeEnv          = awsCollectEnvironmentPrefix + "HISTORY_MAX_AGE"
	awsCollectChangeRetentionEnv = awsCollectEnvironmentPrefix + "HISTORY_CHANGE_RETENTION"
)

func durationOr(value string, fallback time.Duration) time.Duration {
	if parsed, err := time.ParseDuration(strings.TrimSpace(value)); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// InventoryInterval is how often inventory units are re-read.
func (collect AWSCollectConfig) InventoryInterval() time.Duration {
	return durationOr(collect.Intervals.Inventory, DefaultAWSInventoryInterval)
}

// HealthInterval is how often health units are re-read.
func (collect AWSCollectConfig) HealthInterval() time.Duration {
	return durationOr(collect.Intervals.Health, DefaultAWSHealthInterval)
}

// ChangesInterval is how often CloudTrail is polled.
func (collect AWSCollectConfig) ChangesInterval() time.Duration {
	return durationOr(collect.Intervals.Changes, DefaultAWSChangesInterval)
}

// MaxGenerations is how many generations of one unit the file store keeps.
func (collect AWSCollectConfig) MaxGenerations() int {
	if collect.History.MaxGenerations > 0 {
		return collect.History.MaxGenerations
	}
	return DefaultAWSHistoryGenerations
}

// MaxAge is how long a superseded generation is kept by the file store.
func (collect AWSCollectConfig) MaxAge() time.Duration {
	return durationOr(collect.History.MaxAge, DefaultAWSHistoryMaxAge)
}

// ChangeRetention is how long the file store keeps CloudTrail changes.
func (collect AWSCollectConfig) ChangeRetention() time.Duration {
	return durationOr(collect.History.ChangeRetention, DefaultAWSChangeRetention)
}

// DefaultAWSApplicationSources is the resolver order when none is set.
func DefaultAWSApplicationSources() []string {
	return []string{awsApplicationSourceAppReg, awsApplicationSourceStack, awsApplicationSourceTags}
}

// DefaultAWSApplicationTagKeys are the tag keys the tag source reads when
// none are set.
func DefaultAWSApplicationTagKeys() []string {
	return []string{"app", "service"}
}

// EffectiveSources is the configured resolver order or the default.
func (applications AWSApplicationsConfig) EffectiveSources() []string {
	if len(applications.Sources) == 0 {
		return DefaultAWSApplicationSources()
	}
	return append([]string(nil), applications.Sources...)
}

// EffectiveTagKeys is the configured tag keys or the default.
func (applications AWSApplicationsConfig) EffectiveTagKeys() []string {
	if len(applications.TagKeys) == 0 {
		return DefaultAWSApplicationTagKeys()
	}
	return append([]string(nil), applications.TagKeys...)
}

// AWSAccountConfig is one customer account. An empty Regions slice means every
// region the account reports as enabled (the "auto" form).
type AWSAccountConfig struct {
	AccountID   string        `mapstructure:"account_id"`
	DisplayName string        `mapstructure:"display_name"`
	Regions     []string      `mapstructure:"regions"`
	Auth        AWSAuthConfig `mapstructure:"auth"`
	// Owner names who owns the account's cost when no team tag applies.
	Owner string `mapstructure:"owner"`
}

type AWSAuthConfig struct {
	Mode       string `mapstructure:"mode"`
	RoleARN    string `mapstructure:"role_arn"`
	ExternalID string `mapstructure:"external_id"`
}

type AWSTagsConfig struct {
	ValueAllowlist []string `mapstructure:"value_allowlist"`
}

type AWSBudgetsConfig struct {
	MaxRequestsPerSecond int `mapstructure:"max_requests_per_second"`
}

type AWSOptionalDataConfig struct {
	CostExplorer   bool `mapstructure:"cost_explorer"`
	SecurityHub    bool `mapstructure:"security_hub"`
	Config         bool `mapstructure:"config"`
	TrustedAdvisor bool `mapstructure:"trusted_advisor"`
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
	ChangeCapture        string                      `mapstructure:"change_capture"`
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
	connectors := ConnectorsConfig{
		Kubernetes: KubernetesConnectorConfig{KubernetesToolConfig: KubernetesToolConfig{ChangeCapture: "continuous"}},
		AWS:        AWSConnectorConfig{Collect: AWSCollectConfig{Enable: true}},
	}
	if value == nil {
		return connectors, nil
	}
	if err := validateAWSLiteralSecrets(value); err != nil {
		return connectors, err
	}
	value = expandEnvironmentScalars(value)
	if err := validateMultipleShape(value); err != nil {
		return connectors, err
	}
	if err := normalizeConnectorCIDRs(value); err != nil {
		return connectors, err
	}
	if err := normalizeAWSRegions(value); err != nil {
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
	normalizeAWSDefaults(&connectors.AWS)
	if err := validateAWSConnector(connectors.AWS); err != nil {
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
	for _, entry := range connector.Resolved() {
		if entry.ChangeCapture != "" && entry.ChangeCapture != "continuous" && entry.ChangeCapture != "demand" {
			return errors.New("connectors.kubernetes.change_capture must be continuous or demand")
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

// awsPlaceholderPattern is the only accepted form of an external ID: a
// reference to an environment variable. A literal would persist a secret in a
// file and is rejected before environment expansion.
var awsPlaceholderPattern = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

func foldedField(fields map[string]any, name string) (string, any, bool) {
	for key, value := range fields {
		if strings.EqualFold(key, name) {
			return key, value, true
		}
	}
	return "", nil, false
}

func validateAWSLiteralSecrets(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	_, awsValue, present := foldedField(root, "aws")
	if !present {
		return nil
	}
	awsFields, ok := awsValue.(map[string]any)
	if !ok {
		return nil
	}
	_, accountsValue, present := foldedField(awsFields, "accounts")
	if !present {
		return nil
	}
	entries, ok := accountsValue.([]any)
	if !ok {
		return nil
	}
	for index, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		_, authValue, present := foldedField(fields, "auth")
		if !present {
			continue
		}
		authFields, ok := authValue.(map[string]any)
		if !ok {
			continue
		}
		_, externalValue, present := foldedField(authFields, "external_id")
		if !present {
			continue
		}
		placeholder, ok := externalValue.(string)
		if !ok || !awsPlaceholderPattern.MatchString(placeholder) {
			return fmt.Errorf("connectors.aws.accounts[%d]: external_id must reference an environment variable", index)
		}
	}
	return nil
}

func normalizeAWSRegions(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	_, awsValue, present := foldedField(root, "aws")
	if !present {
		return nil
	}
	awsFields, ok := awsValue.(map[string]any)
	if !ok {
		return nil
	}
	_, accountsValue, present := foldedField(awsFields, "accounts")
	if !present {
		return nil
	}
	entries, ok := accountsValue.([]any)
	if !ok {
		return nil
	}
	for index, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		key, regionsValue, present := foldedField(fields, "regions")
		if !present || regionsValue == nil {
			continue
		}
		switch typed := regionsValue.(type) {
		case string:
			if !strings.EqualFold(strings.TrimSpace(typed), "auto") {
				return fmt.Errorf("connectors.aws.accounts[%d]: regions must be auto or a list", index)
			}
			fields[key] = []any{}
		case []any:
			if len(typed) == 0 {
				return fmt.Errorf("connectors.aws.accounts[%d]: regions list is empty", index)
			}
			for _, item := range typed {
				region, ok := item.(string)
				if !ok || awsauth.ValidateRegion(strings.TrimSpace(region)) != nil {
					return fmt.Errorf("connectors.aws.accounts[%d]: invalid region", index)
				}
			}
		default:
			return fmt.Errorf("connectors.aws.accounts[%d]: regions must be auto or a list", index)
		}
	}
	return nil
}

func normalizeAWSDefaults(connector *AWSConnectorConfig) {
	normalizeAWSFinOpsDefaults(&connector.FinOps)
	if connector.Budgets.MaxRequestsPerSecond == 0 {
		connector.Budgets.MaxRequestsPerSecond = 2
	}
	if connector.HealthCheckInterval == "" {
		connector.HealthCheckInterval = "5m"
	}
	collect := &connector.Collect
	if collect.Intervals.Inventory == "" {
		collect.Intervals.Inventory = DefaultAWSInventoryInterval.String()
	}
	if collect.Intervals.Health == "" {
		collect.Intervals.Health = DefaultAWSHealthInterval.String()
	}
	if collect.Intervals.Changes == "" {
		collect.Intervals.Changes = DefaultAWSChangesInterval.String()
	}
	if collect.History.MaxGenerations == 0 {
		collect.History.MaxGenerations = DefaultAWSHistoryGenerations
	}
	if collect.History.MaxAge == "" {
		collect.History.MaxAge = DefaultAWSHistoryMaxAge.String()
	}
	if collect.History.ChangeRetention == "" {
		collect.History.ChangeRetention = DefaultAWSChangeRetention.String()
	}
	if len(connector.Applications.Sources) == 0 {
		connector.Applications.Sources = DefaultAWSApplicationSources()
	}
	if len(connector.Applications.TagKeys) == 0 {
		connector.Applications.TagKeys = DefaultAWSApplicationTagKeys()
	}
}

// validateAWSCollect checks the collector block. Messages name the field,
// never the rejected value.
func validateAWSCollect(collect AWSCollectConfig, applications AWSApplicationsConfig) error {
	intervals := map[string]string{
		"inventory": collect.Intervals.Inventory,
		"health":    collect.Intervals.Health,
		"changes":   collect.Intervals.Changes,
	}
	for _, name := range []string{"inventory", "health", "changes"} {
		value := intervals[name]
		if value == "" {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < minAWSCollectInterval || parsed > maxAWSCollectInterval {
			return fmt.Errorf("connectors.aws.collect.intervals.%s must be between 30s and 24h", name)
		}
	}
	if collect.History.MaxGenerations < 0 || collect.History.MaxGenerations > maxAWSHistoryGenerations {
		return errors.New("connectors.aws.collect.history.max_generations must be between 1 and 10000")
	}
	for name, value := range map[string]string{"max_age": collect.History.MaxAge, "change_retention": collect.History.ChangeRetention} {
		if value == "" {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < minAWSHistoryAge || parsed > maxAWSHistoryAge {
			return fmt.Errorf("connectors.aws.collect.history.%s must be between 1h and 8784h", name)
		}
	}
	seen := map[string]bool{}
	for _, source := range applications.Sources {
		switch source {
		case awsApplicationSourceAppReg, awsApplicationSourceStack, awsApplicationSourceTags:
		default:
			return errors.New("connectors.aws.applications.sources may list appregistry, cloudformation and tags")
		}
		if seen[source] {
			return errors.New("connectors.aws.applications.sources lists a source twice")
		}
		seen[source] = true
	}
	if len(applications.TagKeys) > maxAWSApplicationTagKeys {
		return errors.New("connectors.aws.applications.tag_keys must contain at most 16 keys")
	}
	for _, key := range applications.TagKeys {
		if key == "" || len(key) > 128 || strings.ContainsAny(key, "\x00\r\n,") {
			return errors.New("connectors.aws.applications.tag_keys contains an invalid tag key")
		}
	}
	return nil
}

// applyAWSCollectEnvironment applies the VERSUS_CONNECTORS_AWS_COLLECT_*
// overrides on top of the loaded file and validates the result. An invalid
// override fails the load without echoing the value.
func applyAWSCollectEnvironment(connector *AWSConnectorConfig) error {
	set := false
	if value := os.Getenv(awsCollectEnableEnvironment); value != "" {
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s must be true or false", awsCollectEnableEnvironment)
		}
		connector.Collect.Enable = enabled
	}
	for name, target := range map[string]*string{
		awsCollectInventoryEnv:       &connector.Collect.Intervals.Inventory,
		awsCollectHealthEnv:          &connector.Collect.Intervals.Health,
		awsCollectChangesEnv:         &connector.Collect.Intervals.Changes,
		awsCollectMaxAgeEnv:          &connector.Collect.History.MaxAge,
		awsCollectChangeRetentionEnv: &connector.Collect.History.ChangeRetention,
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*target = value
			set = true
		}
	}
	if value := strings.TrimSpace(os.Getenv(awsCollectGenerationsEnv)); value != "" {
		generations, err := strconv.Atoi(value)
		if err != nil || generations < 1 {
			return fmt.Errorf("%s must be a positive integer", awsCollectGenerationsEnv)
		}
		connector.Collect.History.MaxGenerations = generations
		set = true
	}
	if !set {
		return nil
	}
	return validateAWSCollect(connector.Collect, connector.Applications)
}

func validateAWSConnector(connector AWSConnectorConfig) error {
	if len(connector.Accounts) > 64 {
		return errors.New("connectors.aws.accounts must contain at most 64 entries")
	}
	seen := map[string]bool{}
	for index, account := range connector.Accounts {
		fail := func(reason string) error {
			return fmt.Errorf("connectors.aws.accounts[%d]: %s", index, reason)
		}
		if len(account.AccountID) != 12 || !digitsOnly(account.AccountID) || seen[account.AccountID] {
			return fail("invalid or duplicate account ID")
		}
		seen[account.AccountID] = true
		if len(account.DisplayName) > 128 {
			return fail("display_name exceeds 128 bytes")
		}
		for _, region := range account.Regions {
			if awsauth.ValidateRegion(region) != nil {
				return fail("invalid region")
			}
		}
		mode := strings.ToLower(strings.TrimSpace(account.Auth.Mode))
		switch mode {
		case "", "default_chain", "profile", "web_identity":
			if account.Auth.RoleARN != "" || account.Auth.ExternalID != "" {
				return fail("role fields require assume_role mode")
			}
		case "assume_role":
			if awsauth.ValidateRoleARN(account.Auth.RoleARN) != nil {
				return fail("invalid role ARN")
			}
			if len(account.Auth.ExternalID) > 1224 || strings.ContainsAny(account.Auth.ExternalID, "\x00\r\n") {
				return fail("invalid external ID")
			}
		default:
			return fail("invalid authentication mode")
		}
	}
	for _, value := range connector.Tags.ValueAllowlist {
		if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n,") {
			return errors.New("connectors.aws.tags.value_allowlist contains an invalid tag key")
		}
	}
	if connector.Budgets.MaxRequestsPerSecond < 1 || connector.Budgets.MaxRequestsPerSecond > 1000 {
		return errors.New("connectors.aws.budgets.max_requests_per_second must be between 1 and 1000")
	}
	interval, err := time.ParseDuration(connector.HealthCheckInterval)
	if err != nil || interval < 30*time.Second || interval > 24*time.Hour {
		return errors.New("connectors.aws.health_check_interval must be between 30s and 24h")
	}
	for index, account := range connector.Accounts {
		if len(account.Owner) > 128 {
			return fmt.Errorf("connectors.aws.accounts[%d]: owner must be at most 128 characters", index)
		}
	}
	if err := validateAWSFinOps(connector.FinOps); err != nil {
		return err
	}
	return validateAWSCollect(connector.Collect, connector.Applications)
}

func digitsOnly(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
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
	case reflect.Float64:
		return strconv.ParseFloat(value.(string), 64)
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
		if target.Kind() == reflect.Float64 {
			switch reflect.TypeOf(value).Kind() {
			case reflect.Int, reflect.Int64, reflect.Float32:
				return true
			}
		}
		if reflect.TypeOf(value).Kind() == reflect.String && (target.Kind() == reflect.Bool || target.Kind() == reflect.Int || target.Kind() == reflect.Float64) {
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
