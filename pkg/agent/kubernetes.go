package agent

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// NewKubernetesService builds the shared read-only connector for one org scope.
func NewKubernetesService(cfg config.KubernetesToolConfig, scope tenancy.OrgScope) (*kubernetes.Service, error) {
	mode := strings.TrimSpace(cfg.Auth.Mode)
	if mode == "" && !reflect.ValueOf(cfg.Auth).IsZero() {
		return nil, errors.New("kubernetes: auth.mode is required when auth.* is configured")
	}
	if mode == "" && strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, nil
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil && cfg.Timeout != "" {
		return nil, err
	}
	ttl, err := time.ParseDuration(cfg.DiscoveryTTL)
	if err != nil && cfg.DiscoveryTTL != "" {
		return nil, err
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
			return nil, errors.New("kubernetes: legacy token_file conflicts with auth.mode")
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
	return kubernetes.NewService(client, kubernetes.Scope{OrgID: normalized.Write, ClusterID: clusterID, CredentialID: cfg.CredentialID}, ttl), nil
}
