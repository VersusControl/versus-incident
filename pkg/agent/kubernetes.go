package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ConfigureKubernetesIssueAlerts installs an org-aware emission bridge on every cluster.
// The callback belongs to the host; this package never imports services.
func ConfigureKubernetesIssueAlerts(registry *kubernetes.ServiceRegistry, sink func(context.Context, string, kubernetes.IssueAlert) (kubernetes.Outcome, error)) {
	if registry == nil || sink == nil {
		return
	}
	registry.Each(func(service *kubernetes.Service) {
		service.SetIssueAlertSink(func(ctx context.Context, a kubernetes.IssueAlert) (kubernetes.Outcome, error) {
			return sink(ctx, a.OrgID, a)
		})
	})
}

// NewKubernetesService builds the shared read-only connector for one org scope.
func NewKubernetesRegistry(cfg config.KubernetesConnectorConfig, scope tenancy.OrgScope) (*kubernetes.ServiceRegistry, []error) {
	entries := []kubernetes.ClusterEntry{}
	errorsFound := []error{}
	resolved := cfg.Resolved()
	// Each cluster's first credential check may wait on its cloud token host
	// up to its own bound. The clusters are built side by side, so a token
	// host that drops packets delays startup by one bound, not one per cluster.
	type built struct {
		service *kubernetes.Service
		err     error
	}
	results := make([]built, len(resolved))
	var wg sync.WaitGroup
	for i, entry := range resolved {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service, err := NewKubernetesService(entry.KubernetesToolConfig, scope)
			results[i] = built{service: service, err: err}
		}()
	}
	wg.Wait()
	for i, entry := range resolved {
		service, err := results[i].service, results[i].err
		if service == nil && err == nil && !cfg.Multiple {
			continue
		}
		id := entry.ClusterID
		if id == "" {
			id = config.DefaultKubernetesClusterID
		}
		provider := entry.Auth.Mode
		switch provider {
		case "eks", "aks", "gke", "in_cluster":
		default:
			provider = "generic"
		}
		if err != nil {
			errorsFound = append(errorsFound, err)
		}
		entries = append(entries, kubernetes.ClusterEntry{Info: kubernetes.ClusterInfo{ID: id, DisplayName: entry.DisplayName, Provider: provider}, Service: service, Err: err})
	}
	return kubernetes.NewClusterRegistryWithScope(cfg.Multiple, entries, scope), errorsFound
}

func NewKubernetesService(cfg config.KubernetesToolConfig, scope tenancy.OrgScope) (*kubernetes.Service, error) {
	if cfg.ChangeCapture != "" && cfg.ChangeCapture != "continuous" && cfg.ChangeCapture != "demand" {
		return nil, kubernetes.NewSetupError("invalid change capture configuration", "Set connectors.kubernetes.change_capture to continuous or demand, or leave it empty.")
	}
	mode := strings.TrimSpace(cfg.Auth.Mode)
	if mode == "" && !reflect.ValueOf(cfg.Auth).IsZero() {
		return nil, kubernetes.NewSetupError("auth.mode is required when auth.* is configured", "Set connectors.kubernetes.auth.mode to the mode the auth settings belong to, then restart Versus.")
	}
	if mode == "" && strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, nil
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil && cfg.Timeout != "" {
		return nil, kubernetes.NewSetupError("invalid timeout configuration", "Set connectors.kubernetes.timeout to a duration such as 10s.")
	}
	ttl, err := time.ParseDuration(cfg.DiscoveryTTL)
	if err != nil && cfg.DiscoveryTTL != "" {
		return nil, kubernetes.NewSetupError("invalid discovery TTL configuration", "Set connectors.kubernetes.discovery_ttl to a duration such as 5m.")
	}
	clientConfig := kubernetes.Config{Endpoint: cfg.Endpoint, TokenFile: cfg.TokenFile, CAFile: cfg.CAFile, ServerName: cfg.ServerName}
	if cfg.CAData != "" {
		clientConfig.CAData, err = kubernetes.DecodeCAData(cfg.CAData)
		if err != nil {
			return nil, err
		}
	}
	if mode != "" {
		if cfg.TokenFile != "" {
			return nil, kubernetes.NewSetupError("legacy token_file conflicts with auth.mode", "Move connectors.kubernetes.token_file to auth.token_file with auth.mode token_file, then restart Versus.")
		}
		auth := cfg.Auth
		clientConfig, err = kubernetes.ResolveAuthentication(kubernetes.AuthOptions{
			Mode: auth.Mode, Endpoint: cfg.Endpoint, CAFile: cfg.CAFile, CAData: cfg.CAData, ServerName: cfg.ServerName,
			Token: auth.Token, TokenFile: auth.TokenFile,
			CertificateFile: auth.ClientCertificate.CertificateFile, KeyFile: auth.ClientCertificate.KeyFile,
			CertificateData: auth.ClientCertificate.CertificateData, KeyData: auth.ClientCertificate.KeyData,
			KubeconfigPath: auth.Kubeconfig.Path, KubeconfigContext: auth.Kubeconfig.Context,
			EKSClusterName: auth.EKS.ClusterName, EKSRegion: auth.EKS.Region, EKSRoleARN: auth.EKS.RoleARN, EKSProfile: auth.EKS.Profile,
			AKSCredentialMode: auth.AKS.CredentialMode, AKSServerID: auth.AKS.ServerID, AKSTenantID: auth.AKS.TenantID, AKSClientID: auth.AKS.ClientID, AKSClientSecret: auth.AKS.ClientSecret, AKSFederatedTokenFile: auth.AKS.FederatedTokenFile, AKSEnvironment: auth.AKS.Environment,
			GKECredentialsFile: auth.GKE.CredentialsFile,
		})
		if err != nil {
			return nil, err
		}
	}
	clientConfig.Timeout = timeout
	clientConfig.AllowLoopback = cfg.AllowLoopback
	clientConfig.AllowPrivateNetworks = cfg.AllowPrivateNetworks
	if mode == "in_cluster" {
		clientConfig.AllowPrivateNetworks = true
	}
	clientConfig.EndpointCIDRs = cfg.EndpointCIDRs
	client, err := kubernetes.NewClient(clientConfig)
	if err != nil {
		return nil, err
	}
	clusterID := strings.TrimSpace(cfg.ClusterID)
	if clusterID == "" {
		clusterID = "default"
	}
	normalized := scope.Normalized()
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: normalized.Write, ClusterID: clusterID, CredentialID: cfg.CredentialID}, ttl)
	if err := service.SetCaptureMode(cfg.ChangeCapture); err != nil {
		return nil, err
	}
	service.SetCollectionScope(cfg.ExcludeResources, cfg.Namespaces)
	return service, nil
}
