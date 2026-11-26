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
	// AnomalyDetection is an opaque settings block for an anomaly-detection
	// extension. It applies to every cluster; a cluster's own block refines
	// it. Versus stores and copies it and reads nothing inside it.
	AnomalyDetection map[string]any `mapstructure:"anomaly_detection"`
}

type KubernetesClusterConfig struct {
	DisplayName          string `mapstructure:"display_name"`
	KubernetesToolConfig `mapstructure:",squash"`
	// AnomalyDetection is this cluster's opaque anomaly-detection block,
	// layered over the connector-level one by the extension that reads it.
	AnomalyDetection map[string]any `mapstructure:"anomaly_detection"`
}

// DefaultKubernetesClusterID is the cluster ID of a single-cluster connector
// configured without cluster_id.
const DefaultKubernetesClusterID = "default"

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
	Changes   AWSCollectChangesConfig   `mapstructure:"changes"`
}

// AWSCollectChangesConfig shapes the CloudTrail change poller and the
// change list. Every key has an environment override.
type AWSCollectChangesConfig struct {
	// Lookback is how far back a first poll reads.
	Lookback string `mapstructure:"lookback"`
	// Overlap is re-read on every poll and not claimed until a later one,
	// because CloudTrail delivers events late.
	Overlap string `mapstructure:"overlap"`
	// PageBudget caps LookupEvents pages per poll; 0 sizes it from the
	// CloudTrail rate and the poll interval.
	PageBudget int `mapstructure:"page_budget"`
	// RoutineEvents (service:Action) are counted, not stored row by row.
	// Unset takes the default list; an empty list counts nothing.
	RoutineEvents []string `mapstructure:"routine_events"`
	// BurstWindow and BurstMin group failed attempts into one burst row.
	BurstWindow string `mapstructure:"burst_window"`
	BurstMin    int    `mapstructure:"burst_min"`
	// FailureKeep is how many failed attempts per principal, error code,
	// event and hour are stored as rows; the rest are counted.
	FailureKeep int `mapstructure:"failure_keep"`
	// ReadCap bounds the rows one change list answer reads.
	ReadCap int `mapstructure:"read_cap"`
}

// AWSCollectIntervalsConfig is how often each kind of unit is re-read.
// Every AWS call is a CloudTrail event the account's GuardDuty, extra trail
// copies and log pipeline price, so the defaults read health often and
// inventory rarely, with the change feed re-reading what changed between
// full reads. An interval set here wins over its default.
type AWSCollectIntervalsConfig struct {
	// Inventory is the full inventory read.
	Inventory string `mapstructure:"inventory"`
	// Health is the cadence of a healthy health unit.
	Health  string `mapstructure:"health"`
	Changes string `mapstructure:"changes"`
	// HealthHot is the cadence of a health unit that is unhealthy or
	// changed in the last hour; never slower than Health.
	HealthHot string `mapstructure:"health_hot"`
	// Identity is the IAM read.
	Identity string `mapstructure:"identity"`
	// Policies is the resource policy read; changes to a policy re-read it
	// in between.
	Policies string `mapstructure:"policies"`
	// CredentialReport and Findings are the cadences of those sources.
	CredentialReport string `mapstructure:"credential_report"`
	Findings         string `mapstructure:"findings"`
	// Trails is the trail read that counts the copies kept of each call.
	Trails string `mapstructure:"trails"`
	// RegionsOff is the cadence of a unit in a Region that is not turned
	// on.
	RegionsOff string `mapstructure:"regions_off"`
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
	DefaultAWSInventoryInterval = 6 * time.Hour
	DefaultAWSHealthInterval    = 10 * time.Minute
	DefaultAWSChangesInterval   = 5 * time.Minute
	// DefaultAWSHealthHotInterval is the health cadence of a unit that is
	// unhealthy or changed in the last hour.
	DefaultAWSHealthHotInterval        = 2 * time.Minute
	DefaultAWSIdentityInterval         = time.Hour
	DefaultAWSPoliciesInterval         = 6 * time.Hour
	DefaultAWSCredentialReportInterval = 4 * time.Hour
	DefaultAWSFindingsInterval         = time.Hour
	DefaultAWSTrailsInterval           = 24 * time.Hour
	DefaultAWSRegionsOffInterval       = 24 * time.Hour
	// DefaultAWSCallsPerDay is the daily call target per account. Over it,
	// inventory and policy reads slow down first; health is protected.
	DefaultAWSCallsPerDay        = 20000
	maxAWSCallsPerDay            = 10000000
	maxAWSCycleBudget            = 100000
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
	awsChangesEnvironmentPrefix  = awsCollectEnvironmentPrefix + "CHANGES_"
	awsChangesLookbackEnv        = awsChangesEnvironmentPrefix + "LOOKBACK"
	awsChangesOverlapEnv         = awsChangesEnvironmentPrefix + "OVERLAP"
	awsChangesPageBudgetEnv      = awsChangesEnvironmentPrefix + "PAGE_BUDGET"
	awsChangesRoutineEventsEnv   = awsChangesEnvironmentPrefix + "ROUTINE_EVENTS"
	awsChangesBurstWindowEnv     = awsChangesEnvironmentPrefix + "BURST_WINDOW"
	awsChangesBurstMinEnv        = awsChangesEnvironmentPrefix + "BURST_MIN"
	awsChangesFailureKeepEnv     = awsChangesEnvironmentPrefix + "FAILURE_KEEP"
	awsChangesReadCapEnv         = awsChangesEnvironmentPrefix + "READ_CAP"
)

// Change poller defaults and bounds.
const (
	DefaultAWSChangeLookback    = 24 * time.Hour
	DefaultAWSChangeOverlap     = 15 * time.Minute
	DefaultAWSChangeBurstWindow = 10 * time.Minute
	DefaultAWSChangeBurstMin    = 5
	DefaultAWSChangeFailureKeep = 20
	DefaultAWSChangeReadCap     = 20000
	minAWSChangeLookback        = time.Hour
	maxAWSChangeLookback        = 14 * 24 * time.Hour
	minAWSChangeOverlap         = 5 * time.Minute
	maxAWSChangeOverlap         = 2 * time.Hour
	maxAWSChangePageBudget      = 1000
	maxAWSRoutineEvents         = 32
	minAWSChangeBurstWindow     = time.Minute
	maxAWSChangeBurstWindow     = time.Hour
	minAWSChangeBurstMin        = 2
	maxAWSChangeBurstMin        = 1000
	minAWSChangeFailureKeep     = 5
	maxAWSChangeFailureKeep     = 1000
	minAWSChangeReadCap         = 1000
	maxAWSChangeReadCap         = 200000
)

// DefaultAWSRoutineEvents are the agent and scheduler events counted, not
// stored row by row.
func DefaultAWSRoutineEvents() []string {
	return []string{"ssm:UpdateInstanceInformation", "ecs:SubmitTaskStateChange", "ecs:SubmitContainerStateChange", "logs:CreateLogStream", "config:PutEvaluations"}
}

var awsRoutineEventShape = regexp.MustCompile(`^[a-z0-9-]{1,40}:[A-Za-z0-9]{1,128}$`)

// HistoryHorizonWarning names a history setting that keeps less than the
// 24 h the OSS compare and alarm line promise, or is empty. It warns rather
// than fails: the deployment still works with shorter history and says
// where history starts.
func (collect AWSCollectConfig) HistoryHorizonWarning() string {
	if collect.MaxAge() < 24*time.Hour {
		return "connectors.aws.collect.history.max_age keeps less than 24h of snapshots; compare and the alarm line will show where history starts"
	}
	return ""
}

// ChangeLookback is how far back a first CloudTrail poll reads.
func (collect AWSCollectConfig) ChangeLookback() time.Duration {
	return durationOr(collect.Changes.Lookback, DefaultAWSChangeLookback)
}

// ChangeOverlap is the unsettled tail each poll re-reads.
func (collect AWSCollectConfig) ChangeOverlap() time.Duration {
	return durationOr(collect.Changes.Overlap, DefaultAWSChangeOverlap)
}

// ChangeBurstWindow is the gap that still joins two failed attempts.
func (collect AWSCollectConfig) ChangeBurstWindow() time.Duration {
	return durationOr(collect.Changes.BurstWindow, DefaultAWSChangeBurstWindow)
}

// ChangeBurstMin is the fewest failed attempts that form a burst.
func (collect AWSCollectConfig) ChangeBurstMin() int {
	if collect.Changes.BurstMin > 0 {
		return collect.Changes.BurstMin
	}
	return DefaultAWSChangeBurstMin
}

// ChangeFailureKeep is how many failed attempts per key and hour are rows.
func (collect AWSCollectConfig) ChangeFailureKeep() int {
	if collect.Changes.FailureKeep > 0 {
		return collect.Changes.FailureKeep
	}
	return DefaultAWSChangeFailureKeep
}

// ChangeReadCap bounds the rows one change list answer reads.
func (collect AWSCollectConfig) ChangeReadCap() int {
	if collect.Changes.ReadCap > 0 {
		return collect.Changes.ReadCap
	}
	return DefaultAWSChangeReadCap
}

// ChangeRoutineEvents is the routine list: the configured one, or the
// default when unset.
func (collect AWSCollectConfig) ChangeRoutineEvents() []string {
	if collect.Changes.RoutineEvents == nil {
		return DefaultAWSRoutineEvents()
	}
	return append([]string{}, collect.Changes.RoutineEvents...)
}

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

// HealthHotInterval is how often a health unit that is unhealthy or
// changed in the last hour is re-read; never slower than HealthInterval.
// A user who set health and not health_hot gets no fast cadence: their
// interval wins.
func (collect AWSCollectConfig) HealthHotInterval() time.Duration {
	if collect.Intervals.HealthHot == "" && collect.Intervals.Health != "" {
		return collect.HealthInterval()
	}
	return min(durationOr(collect.Intervals.HealthHot, DefaultAWSHealthHotInterval), collect.HealthInterval())
}

// IdentityInterval is how often the IAM read runs.
func (collect AWSCollectConfig) IdentityInterval() time.Duration {
	return durationOr(collect.Intervals.Identity, DefaultAWSIdentityInterval)
}

// PoliciesInterval is how often the resource policy reads run. Unset, it
// follows an inventory interval slower than its default.
func (collect AWSCollectConfig) PoliciesInterval() time.Duration {
	return durationOr(collect.Intervals.Policies, max(DefaultAWSPoliciesInterval, collect.InventoryInterval()))
}

// CredentialReportInterval is how often the credential report is read.
func (collect AWSCollectConfig) CredentialReportInterval() time.Duration {
	return durationOr(collect.Intervals.CredentialReport, DefaultAWSCredentialReportInterval)
}

// FindingsInterval is how often imported findings are read.
func (collect AWSCollectConfig) FindingsInterval() time.Duration {
	return durationOr(collect.Intervals.Findings, DefaultAWSFindingsInterval)
}

// TrailsInterval is how often the account's trails are read.
func (collect AWSCollectConfig) TrailsInterval() time.Duration {
	return durationOr(collect.Intervals.Trails, DefaultAWSTrailsInterval)
}

// RegionsOffInterval is how often a unit in a Region that is not turned on
// is tried again.
func (collect AWSCollectConfig) RegionsOffInterval() time.Duration {
	return durationOr(collect.Intervals.RegionsOff, DefaultAWSRegionsOffInterval)
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
	// PerCycle caps the AWS calls one run of a source makes, per account
	// and Region (pages for findings): health, inventory, changes,
	// identity, policies, credential_report, findings. A source left out
	// takes its default. Over it, the run stops, keeps its last good data
	// and shows the rest as capped.
	PerCycle map[string]int `mapstructure:"per_cycle"`
	// CallsPerDay is the daily call target per account. Over it, inventory
	// and policy reads slow down first and the slowdown shows in Settings.
	CallsPerDay int `mapstructure:"calls_per_day"`
}

// DefaultAWSCycleBudgets are the per-run call budgets of each source.
func DefaultAWSCycleBudgets() map[string]int {
	return map[string]int{
		"health":            200,
		"inventory":         700,
		"changes":           60,
		"identity":          120,
		"policies":          250,
		"credential_report": 3,
		"findings":          50,
	}
}

// CycleBudgets returns every source's per-run budget: the configured one,
// else its default.
func (budgets AWSBudgetsConfig) CycleBudgets() map[string]int {
	out := DefaultAWSCycleBudgets()
	for source, value := range budgets.PerCycle {
		if _, known := out[source]; known && value > 0 {
			out[source] = value
		}
	}
	return out
}

// KubernetesAnomalyDetectionBlocks is the anomaly_detection settings that
// apply to one cluster, as two separate layers.
type KubernetesAnomalyDetectionBlocks struct {
	// ClusterID is the cluster's ID; DefaultKubernetesClusterID for a
	// single-cluster connector without cluster_id.
	ClusterID string
	// Connector is the connector-level block, nil when absent.
	Connector map[string]any
	// Cluster is the cluster's own block, nil when absent. Always nil for a
	// single-cluster connector, whose only block is the connector-level one.
	Cluster map[string]any
}

// AnomalyDetectionBlocks returns, for each resolved cluster in order, the
// connector-level and cluster-level anomaly_detection blocks, unmerged. The
// maps are deep copies, so a caller may change them freely.
func (connector KubernetesConnectorConfig) AnomalyDetectionBlocks() []KubernetesAnomalyDetectionBlocks {
	resolved := connector.Resolved()
	out := make([]KubernetesAnomalyDetectionBlocks, len(resolved))
	for index, entry := range resolved {
		id := entry.ClusterID
		if id == "" {
			id = DefaultKubernetesClusterID
		}
		out[index] = KubernetesAnomalyDetectionBlocks{
			ClusterID: id,
			Connector: cloneAnyMap(connector.AnomalyDetection),
			Cluster:   cloneAnyMap(entry.AnomalyDetection),
		}
	}
	return out
}

// DailyCallTarget is the daily call target per account.
func (budgets AWSBudgetsConfig) DailyCallTarget() int {
	if budgets.CallsPerDay > 0 {
		return budgets.CallsPerDay
	}
	return DefaultAWSCallsPerDay
}

type AWSOptionalDataConfig struct {
	CostExplorer   bool `mapstructure:"cost_explorer"`
	SecurityHub    bool `mapstructure:"security_hub"`
	Config         bool `mapstructure:"config"`
	TrustedAdvisor bool `mapstructure:"trusted_advisor"`
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
			if multiple && !strings.EqualFold(name, "multiple") && !strings.EqualFold(name, "clusters") && !strings.EqualFold(name, "anomaly_detection") {
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
	if connector.Budgets.CallsPerDay == 0 {
		connector.Budgets.CallsPerDay = DefaultAWSCallsPerDay
	}
	if connector.HealthCheckInterval == "" {
		connector.HealthCheckInterval = "5m"
	}
	collect := &connector.Collect
	// The fast health cadence is set before health takes its default, so
	// an explicit health interval with no health_hot keeps no fast
	// cadence.
	if collect.Intervals.HealthHot == "" {
		collect.Intervals.HealthHot = collect.HealthHotInterval().String()
	}
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
	if collect.Changes.Lookback == "" {
		collect.Changes.Lookback = DefaultAWSChangeLookback.String()
	}
	if collect.Changes.Overlap == "" {
		collect.Changes.Overlap = DefaultAWSChangeOverlap.String()
	}
	if collect.Changes.BurstWindow == "" {
		collect.Changes.BurstWindow = DefaultAWSChangeBurstWindow.String()
	}
	if collect.Changes.BurstMin == 0 {
		collect.Changes.BurstMin = DefaultAWSChangeBurstMin
	}
	if collect.Changes.FailureKeep == 0 {
		collect.Changes.FailureKeep = DefaultAWSChangeFailureKeep
	}
	if collect.Changes.ReadCap == 0 {
		collect.Changes.ReadCap = DefaultAWSChangeReadCap
	}
	if collect.Changes.RoutineEvents == nil {
		collect.Changes.RoutineEvents = DefaultAWSRoutineEvents()
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
		"inventory":         collect.Intervals.Inventory,
		"health":            collect.Intervals.Health,
		"changes":           collect.Intervals.Changes,
		"health_hot":        collect.Intervals.HealthHot,
		"identity":          collect.Intervals.Identity,
		"policies":          collect.Intervals.Policies,
		"credential_report": collect.Intervals.CredentialReport,
		"findings":          collect.Intervals.Findings,
		"trails":            collect.Intervals.Trails,
		"regions_off":       collect.Intervals.RegionsOff,
	}
	for _, name := range []string{"inventory", "health", "changes", "health_hot", "identity", "policies", "credential_report", "findings", "trails", "regions_off"} {
		value := intervals[name]
		if value == "" {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < minAWSCollectInterval || parsed > maxAWSCollectInterval {
			return fmt.Errorf("connectors.aws.collect.intervals.%s must be between 30s and 24h", name)
		}
	}
	if collect.Intervals.Policies != "" {
		policies, policiesErr := time.ParseDuration(collect.Intervals.Policies)
		if policiesErr == nil && policies < collect.InventoryInterval() {
			return errors.New("connectors.aws.collect.intervals.policies must not be shorter than intervals.inventory: policy reads are metered and run no more often than the full inventory")
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
	if err := validateAWSChanges(collect.Changes); err != nil {
		return err
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

// validateAWSChanges checks the change poller block against its bounds.
func validateAWSChanges(changes AWSCollectChangesConfig) error {
	durations := []struct {
		name, value string
		low, high   time.Duration
		bounds      string
	}{
		{"lookback", changes.Lookback, minAWSChangeLookback, maxAWSChangeLookback, "1h and 336h"},
		{"overlap", changes.Overlap, minAWSChangeOverlap, maxAWSChangeOverlap, "5m and 2h"},
		{"burst_window", changes.BurstWindow, minAWSChangeBurstWindow, maxAWSChangeBurstWindow, "1m and 1h"},
	}
	for _, item := range durations {
		if item.value == "" {
			continue
		}
		parsed, err := time.ParseDuration(item.value)
		if err != nil || parsed < item.low || parsed > item.high {
			return fmt.Errorf("connectors.aws.collect.changes.%s must be between %s", item.name, item.bounds)
		}
	}
	if changes.PageBudget < 0 || changes.PageBudget > maxAWSChangePageBudget {
		return errors.New("connectors.aws.collect.changes.page_budget must be between 0 (automatic) and 1000")
	}
	if changes.BurstMin != 0 && (changes.BurstMin < minAWSChangeBurstMin || changes.BurstMin > maxAWSChangeBurstMin) {
		return errors.New("connectors.aws.collect.changes.burst_min must be between 2 and 1000")
	}
	if changes.FailureKeep != 0 && (changes.FailureKeep < minAWSChangeFailureKeep || changes.FailureKeep > maxAWSChangeFailureKeep) {
		return errors.New("connectors.aws.collect.changes.failure_keep must be between 5 and 1000")
	}
	if changes.ReadCap != 0 && (changes.ReadCap < minAWSChangeReadCap || changes.ReadCap > maxAWSChangeReadCap) {
		return errors.New("connectors.aws.collect.changes.read_cap must be between 1000 and 200000")
	}
	if len(changes.RoutineEvents) > maxAWSRoutineEvents {
		return errors.New("connectors.aws.collect.changes.routine_events must contain at most 32 entries")
	}
	for _, event := range changes.RoutineEvents {
		if !awsRoutineEventShape.MatchString(event) {
			return errors.New("connectors.aws.collect.changes.routine_events entries must look like service:Action")
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
		awsChangesLookbackEnv:        &connector.Collect.Changes.Lookback,
		awsChangesOverlapEnv:         &connector.Collect.Changes.Overlap,
		awsChangesBurstWindowEnv:     &connector.Collect.Changes.BurstWindow,
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
	for name, target := range map[string]*int{
		awsChangesPageBudgetEnv:  &connector.Collect.Changes.PageBudget,
		awsChangesBurstMinEnv:    &connector.Collect.Changes.BurstMin,
		awsChangesFailureKeepEnv: &connector.Collect.Changes.FailureKeep,
		awsChangesReadCapEnv:     &connector.Collect.Changes.ReadCap,
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				return fmt.Errorf("%s must be a non-negative integer", name)
			}
			*target = parsed
			set = true
		}
	}
	if value, ok := os.LookupEnv(awsChangesRoutineEventsEnv); ok {
		events := []string{}
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				events = append(events, item)
			}
		}
		connector.Collect.Changes.RoutineEvents = events
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
	if connector.Budgets.CallsPerDay < 0 || connector.Budgets.CallsPerDay > maxAWSCallsPerDay {
		return errors.New("connectors.aws.budgets.calls_per_day must be between 1 and 10000000")
	}
	known := DefaultAWSCycleBudgets()
	for source, value := range connector.Budgets.PerCycle {
		if _, ok := known[source]; !ok {
			return errors.New("connectors.aws.budgets.per_cycle names an unknown source; use health, inventory, changes, identity, policies, credential_report or findings")
		}
		if value < 1 || value > maxAWSCycleBudget {
			return fmt.Errorf("connectors.aws.budgets.per_cycle.%s must be between 1 and 100000", source)
		}
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

// connectorShapeError checks a decoded connector block against its Go type
// and names the first problem by YAML path: an unknown key by its name, a
// wrong type as an invalid value. Values are never included, because a
// connector value may be a credential.
func connectorShapeError(value any, target reflect.Type, path string) error {
	if value == nil {
		return fmt.Errorf("%s: invalid value", path)
	}
	if target == reflect.TypeOf(map[string]any(nil)) {
		return opaqueBlockKeyError(value, path)
	}
	if target.Kind() != reflect.Struct {
		if reflect.TypeOf(value).AssignableTo(target) {
			return nil
		}
		if target.Kind() == reflect.Float64 {
			switch reflect.TypeOf(value).Kind() {
			case reflect.Int, reflect.Int64, reflect.Float32:
				return nil
			}
		}
		if reflect.TypeOf(value).Kind() == reflect.String && (target.Kind() == reflect.Bool || target.Kind() == reflect.Int || target.Kind() == reflect.Float64) {
			if _, err := connectorScalar(reflect.TypeOf(value), target, value); err == nil {
				return nil
			}
			return fmt.Errorf("%s: invalid value", path)
		}
		if target.Kind() == reflect.Slice {
			return connectorSliceError(value, target, path)
		}
		if target.Kind() == reflect.Map && target.Key().Kind() == reflect.String {
			return connectorMapError(value, target, path)
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

// opaqueBlockKeyError checks a free-form settings block: it must be a
// mapping, and every mapping inside it, at any depth, must have text keys, so
// the block decodes the same way wherever it is read. The first offending key
// is named by path; values are never echoed.
func opaqueBlockKeyError(value any, path string) error {
	if _, ok := value.(map[string]any); !ok {
		if _, mixed := value.(map[any]any); !mixed {
			return fmt.Errorf("%s: invalid value", path)
		}
	}
	return opaqueValueKeyError(value, path)
}

func opaqueValueKeyError(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := opaqueValueKeyError(typed[key], path+"."+connectorKeyForError(key)); err != nil {
				return err
			}
		}
	case map[any]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if _, text := key.(string); !text {
				keys = append(keys, connectorKeyForError(fmt.Sprint(key)))
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			return fmt.Errorf("%s: key %s must be text; quote it", path, keys[0])
		}
	case []any:
		for index, item := range typed {
			if err := opaqueValueKeyError(item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
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

// connectorMapError checks every value of a map keyed by name, such as
// the per-source budgets.
func connectorMapError(value any, target reflect.Type, path string) error {
	entries, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: invalid value", path)
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := connectorShapeError(entries[key], target.Elem(), path+"."+connectorKeyForError(key)); err != nil {
			return err
		}
	}
	return nil
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
