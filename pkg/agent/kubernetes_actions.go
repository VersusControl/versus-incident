package agent

import (
	"errors"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	k8sactions "github.com/VersusControl/versus-incident/pkg/agent/act/adapters/k8s"
	"github.com/VersusControl/versus-incident/pkg/config"
	kubernetes "github.com/VersusControl/versus-incident/pkg/kubernetes"
)

func buildKubernetesActionAdapters(cfg config.KubernetesToolConfig) ([]act.Adapter, error) {
	actions := cfg.Actions
	if !actions.Enable {
		return nil, nil
	}
	mode := strings.TrimSpace(actions.Auth.Mode)
	if mode == "" {
		return nil, errors.New("kubernetes actions require their own explicit auth.mode")
	}
	if mode == "in_cluster" {
		return nil, errors.New("kubernetes actions require a separate actor credential, not the pod ServiceAccount")
	}
	endpoint, caFile, caData, serverName := cfg.Endpoint, cfg.CAFile, cfg.CAData, cfg.ServerName
	if mode == "kubeconfig" {
		endpoint, caFile, caData, serverName = "", "", "", ""
	}
	auth, err := kubernetes.ResolveAuthentication(kubernetes.AuthOptions{
		Mode: mode, Endpoint: endpoint, CAFile: caFile, CAData: caData, ServerName: serverName,
		Token: actions.Auth.Token, TokenFile: actions.Auth.TokenFile,
		CertificateFile: actions.Auth.ClientCertificate.CertificateFile, KeyFile: actions.Auth.ClientCertificate.KeyFile,
		CertificateData: actions.Auth.ClientCertificate.CertificateData, KeyData: actions.Auth.ClientCertificate.KeyData,
		KubeconfigPath: actions.Auth.Kubeconfig.Path, KubeconfigContext: actions.Auth.Kubeconfig.Context,
		EKSClusterName: actions.Auth.EKS.ClusterName, EKSRegion: actions.Auth.EKS.Region, EKSRoleARN: actions.Auth.EKS.RoleARN, EKSProfile: actions.Auth.EKS.Profile,
		AKSCredentialMode: actions.Auth.AKS.CredentialMode, AKSServerID: actions.Auth.AKS.ServerID, AKSTenantID: actions.Auth.AKS.TenantID, AKSClientID: actions.Auth.AKS.ClientID, AKSClientSecret: actions.Auth.AKS.ClientSecret, AKSFederatedTokenFile: actions.Auth.AKS.FederatedTokenFile, AKSEnvironment: actions.Auth.AKS.Environment,
		GKECredentialsFile: actions.Auth.GKE.CredentialsFile,
	})
	if err != nil {
		return nil, err
	}
	timeout := actions.Timeout
	if timeout == "" {
		timeout = cfg.Timeout
	}
	parsedTimeout, err := time.ParseDuration(timeout)
	if err != nil && timeout != "" {
		return nil, err
	}
	api, err := k8sactions.NewClient(k8sactions.ClientConfig{
		Endpoint: auth.Endpoint, CAFile: auth.CAFile, CAData: auth.CAData, ServerName: auth.ServerName,
		Credentials: auth.Credentials, Timeout: parsedTimeout, AllowLoopback: cfg.AllowLoopback,
		AllowPrivateNetworks: cfg.AllowPrivateNetworks || auth.AllowPrivateNetworks, EndpointCIDRs: cfg.EndpointCIDRs,
	})
	if err != nil {
		return nil, err
	}
	cluster := cfg.ClusterID
	if cluster == "" {
		cluster = "default"
	}
	return k8sactions.NewAdapters(api, k8sactions.Options{Cluster: cluster, MaxReplicas: actions.MaxReplicas}), nil
}
