package kubernetes

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"
)

const maxKubeconfigBytes = int64(1 << 20)

var (
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// AuthOptions contains the supported, provider-neutral Kubernetes auth inputs.
type AuthOptions struct {
	Mode, Endpoint, CAFile, CAData, ServerName, Token, TokenFile              string
	CertificateFile, KeyFile, CertificateData, KeyData                        string
	KubeconfigPath, KubeconfigContext                                         string
	EKSClusterName, EKSRegion, EKSRoleARN, EKSProfile                         string
	AKSCredentialMode, AKSServerID, AKSTenantID, AKSClientID, AKSClientSecret string
	AKSFederatedTokenFile, AKSEnvironment, GKECredentialsFile                 string
}

// ResolveAuthentication resolves one explicit auth mode into the existing
// stdlib HTTP client configuration.
func ResolveAuthentication(options AuthOptions) (Config, error) {
	mode := strings.TrimSpace(options.Mode)
	result := Config{Endpoint: strings.TrimSpace(options.Endpoint), CAFile: strings.TrimSpace(options.CAFile), ServerName: strings.TrimSpace(options.ServerName)}
	var err error
	if result.CAFile != "" && options.CAData != "" {
		return Config{}, setupError("conflicting certificate authority configuration", "Set only one of connectors.kubernetes.ca_file or ca_data, then restart Versus.")
	}
	if options.CAData != "" {
		result.CAData, err = DecodeCAData(options.CAData)
		if err != nil {
			return Config{}, err
		}
	}
	switch mode {
	case "in_cluster":
		if result.Endpoint != "" || result.CAFile != "" || len(result.CAData) > 0 || result.ServerName != "" || hasExplicitCredential(options) {
			return Config{}, setupError("conflicting in-cluster configuration", "With auth.mode in_cluster, remove endpoint, ca_file, ca_data, server_name and every explicit credential, then restart Versus.")
		}
		host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
		port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
		if port == "" {
			port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
		}
		if port == "" {
			port = "443"
		}
		if host == "" {
			return Config{}, setupError("in-cluster environment unavailable", "auth.mode in_cluster works only inside a Kubernetes Pod. Outside a cluster, use auth.mode token_file, client_certificate, kubeconfig, eks, aks or gke.")
		}
		result.Endpoint = "https://" + net.JoinHostPort(host, port)
		result.CAFile = inClusterCAFile
		result.AllowPrivateNetworks = true
		result.Credentials = tokenFileCredential(inClusterTokenFile)
	case "token":
		if options.TokenFile != "" || hasNonTokenCredential(options) {
			return Config{}, setupError("conflicting credential configuration", "Configure exactly one credential for the selected connectors.kubernetes.auth.mode, then restart Versus.")
		}
		credential, err := staticTokenCredential(options.Token)
		if err != nil {
			return Config{}, err
		}
		result.Credentials = credential
	case "token_file":
		if options.Token != "" || options.TokenFile == "" || hasNonTokenCredential(options) {
			return Config{}, setupError("conflicting credential configuration", "Configure exactly one credential for the selected connectors.kubernetes.auth.mode, then restart Versus.")
		}
		result.Credentials = tokenFileCredential(options.TokenFile)
	case "client_certificate":
		if options.Token != "" || options.TokenFile != "" || hasCloudCredential(options) || hasKubeconfigCredential(options) {
			return Config{}, setupError("conflicting credential configuration", "Configure exactly one credential for the selected connectors.kubernetes.auth.mode, then restart Versus.")
		}
		certificateData, err := decodeCredentialData(options.CertificateData)
		if err != nil {
			return Config{}, err
		}
		keyData, err := decodeCredentialData(options.KeyData)
		if err != nil {
			return Config{}, err
		}
		result.Credentials, err = clientCertificateCredential(options.CertificateFile, options.KeyFile, certificateData, keyData)
		if err != nil {
			return Config{}, err
		}
	case "kubeconfig":
		if result.Endpoint != "" || result.CAFile != "" || len(result.CAData) > 0 || result.ServerName != "" || options.KubeconfigPath == "" || options.Token != "" || options.TokenFile != "" || hasClientCertificateCredential(options) || hasCloudCredential(options) {
			return Config{}, setupError("conflicting kubeconfig configuration", "With auth.mode kubeconfig, set auth.kubeconfig.path and remove endpoint, CA, server_name and every other credential, then restart Versus.")
		}
		return loadKubeconfig(options.KubeconfigPath, options.KubeconfigContext)
	case "eks":
		if options.EKSClusterName == "" || hasDirectCredential(options) || hasAKSCredential(options) || options.GKECredentialsFile != "" {
			return Config{}, setupError("invalid EKS configuration", "Set auth.eks.cluster_name and remove credentials of other modes, then restart Versus.")
		}
		result.Credentials, err = newEKSCredentialSource(options)
	case "aks":
		if hasDirectCredential(options) || hasEKSCredential(options) || options.GKECredentialsFile != "" {
			return Config{}, setupError("invalid AKS configuration", "Remove credentials of other modes from the AKS connector, then restart Versus.")
		}
		result.Credentials, err = newAKSCredentialSource(options)
	case "gke":
		if hasDirectCredential(options) || hasEKSCredential(options) || hasAKSCredential(options) {
			return Config{}, setupError("invalid GKE configuration", "Remove credentials of other modes from the GKE connector, then restart Versus.")
		}
		result.Credentials, err = newGKECredentialSource(options.GKECredentialsFile)
	default:
		return Config{}, setupError("unsupported authentication mode", "Set connectors.kubernetes.auth.mode to in_cluster, token, token_file, client_certificate, kubeconfig, eks, aks or gke.")
	}
	if err != nil {
		return Config{}, err
	}
	if result.Endpoint == "" {
		return Config{}, ErrInvalidEndpoint
	}
	return result, nil
}

type kubeconfigDocument struct {
	APIVersion     string           `yaml:"apiVersion"`
	Kind           string           `yaml:"kind"`
	Preferences    kubePreferences  `yaml:"preferences"`
	CurrentContext string           `yaml:"current-context"`
	Clusters       []namedCluster   `yaml:"clusters"`
	Contexts       []namedContext   `yaml:"contexts"`
	Users          []namedUser      `yaml:"users"`
	Extensions     []namedExtension `yaml:"extensions"`
}
type kubePreferences struct {
	Colors     bool             `yaml:"colors"`
	Extensions []namedExtension `yaml:"extensions"`
}
type namedExtension struct {
	Name      string `yaml:"name"`
	Extension any    `yaml:"extension"`
}
type namedCluster struct {
	Name    string      `yaml:"name"`
	Cluster kubeCluster `yaml:"cluster"`
}
type kubeCluster struct {
	Server             string           `yaml:"server"`
	CAFile             string           `yaml:"certificate-authority"`
	CAData             string           `yaml:"certificate-authority-data"`
	Insecure           bool             `yaml:"insecure-skip-tls-verify"`
	ProxyURL           string           `yaml:"proxy-url"`
	TLSServerName      string           `yaml:"tls-server-name"`
	DisableCompression bool             `yaml:"disable-compression"`
	Extensions         []namedExtension `yaml:"extensions"`
}
type namedContext struct {
	Name    string      `yaml:"name"`
	Context kubeContext `yaml:"context"`
}
type kubeContext struct {
	Cluster    string           `yaml:"cluster"`
	User       string           `yaml:"user"`
	Namespace  string           `yaml:"namespace"`
	Extensions []namedExtension `yaml:"extensions"`
}
type namedUser struct {
	Name string   `yaml:"name"`
	User kubeUser `yaml:"user"`
}
type kubeUser struct {
	Token                 string           `yaml:"token"`
	TokenFile             string           `yaml:"tokenFile"`
	ClientCertificate     string           `yaml:"client-certificate"`
	ClientKey             string           `yaml:"client-key"`
	ClientCertificateData string           `yaml:"client-certificate-data"`
	ClientKeyData         string           `yaml:"client-key-data"`
	Username              string           `yaml:"username"`
	Password              string           `yaml:"password"`
	AuthProvider          map[string]any   `yaml:"auth-provider,omitempty"`
	As                    string           `yaml:"as"`
	AsGroups              []string         `yaml:"as-groups"`
	Exec                  *kubeExec        `yaml:"exec,omitempty"`
	Extensions            []namedExtension `yaml:"extensions"`
}
type kubeExec struct {
	APIVersion         string        `yaml:"apiVersion"`
	Command            string        `yaml:"command"`
	Args               []string      `yaml:"args"`
	Env                []kubeExecEnv `yaml:"env"`
	InteractiveMode    string        `yaml:"interactiveMode"`
	ProvideClusterInfo bool          `yaml:"provideClusterInfo"`
	InstallHint        string        `yaml:"installHint"`
}
type kubeExecEnv struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

func loadKubeconfig(path, selectedContext string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o022 != 0 {
		return Config{}, setupError("kubeconfig unavailable or writable by other users", "Mount the kubeconfig at auth.kubeconfig.path and make it writable only by its owner (for example chmod 600), then restart Versus.")
	}
	data, err := readBoundedFile(path, maxKubeconfigBytes)
	if err != nil {
		return Config{}, setupError("kubeconfig unavailable", "Mount a readable kubeconfig at auth.kubeconfig.path, then restart Versus.")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document kubeconfigDocument
	if decoder.Decode(&document) != nil {
		return Config{}, setupError("unsupported kubeconfig", "Use a kubeconfig with apiVersion v1 and kind Config.")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Config{}, setupError("multiple kubeconfig documents are unsupported", "Use a kubeconfig file that holds one YAML document.")
	}
	if document.APIVersion != "v1" || document.Kind != "Config" {
		return Config{}, setupError("unsupported kubeconfig", "Use a kubeconfig with apiVersion v1 and kind Config.")
	}
	if hasDuplicateNames(document.Clusters) || hasDuplicateNames(document.Contexts) || hasDuplicateNames(document.Users) {
		return Config{}, setupError("duplicate kubeconfig entry", "Remove the duplicate cluster, context or user entry from the kubeconfig.")
	}
	if selectedContext == "" {
		selectedContext = document.CurrentContext
	}
	contextEntry, ok := findContext(document.Contexts, selectedContext)
	if !ok {
		return Config{}, setupError("kubeconfig context unavailable", "Set auth.kubeconfig.context to a context the kubeconfig defines, or set its current-context.")
	}
	cluster, ok := findCluster(document.Clusters, contextEntry.Cluster)
	if !ok || cluster.Insecure || cluster.ProxyURL != "" || (cluster.CAFile != "" && cluster.CAData != "") {
		return Config{}, setupError("unsafe kubeconfig cluster", "Remove insecure-skip-tls-verify and proxy-url from the kubeconfig cluster, and set only one of certificate-authority or certificate-authority-data.")
	}
	user, ok := findUser(document.Users, contextEntry.User)
	if !ok {
		return Config{}, setupError("kubeconfig user unavailable", "Make the selected kubeconfig context name a user the kubeconfig defines.")
	}
	if user.Exec != nil || user.AuthProvider != nil {
		return Config{}, setupError("kubeconfig exec and auth-provider credentials are unsupported; configure auth.mode as eks, aks, or gke", "Use auth.mode eks, aks or gke for a cloud cluster, or a kubeconfig user with a token, token file or client certificate.")
	}
	if user.Username != "" || user.Password != "" || user.As != "" || len(user.AsGroups) > 0 {
		return Config{}, setupError("unsupported kubeconfig authentication", "Use a kubeconfig user with a token, token file or client certificate; username, password and impersonation are not supported.")
	}
	base := filepath.Dir(path)
	if contextEntry.Namespace != "" && (!safeSegment(contextEntry.Namespace) || len(contextEntry.Namespace) > 63) {
		return Config{}, ErrInvalidArguments
	}
	result := Config{Endpoint: cluster.Server, ServerName: cluster.TLSServerName, Namespace: contextEntry.Namespace}
	if cluster.CAData != "" {
		result.CAData, err = decodeCredentialData(cluster.CAData)
		if err != nil {
			return Config{}, err
		}
	} else if cluster.CAFile != "" {
		result.CAFile = resolveRelative(base, cluster.CAFile)
	}
	modes := 0
	if user.Token != "" {
		modes++
		result.Credentials, err = staticTokenCredential(user.Token)
	}
	if user.TokenFile != "" {
		modes++
		result.Credentials = tokenFileCredential(resolveRelative(base, user.TokenFile))
	}
	if user.ClientCertificate != "" || user.ClientKey != "" || user.ClientCertificateData != "" || user.ClientKeyData != "" {
		modes++
		certificateData, decodeErr := decodeCredentialData(user.ClientCertificateData)
		if decodeErr != nil {
			return Config{}, decodeErr
		}
		keyData, decodeErr := decodeCredentialData(user.ClientKeyData)
		if decodeErr != nil {
			return Config{}, decodeErr
		}
		result.Credentials, err = clientCertificateCredential(resolveRelative(base, user.ClientCertificate), resolveRelative(base, user.ClientKey), certificateData, keyData)
	}
	if err != nil || modes != 1 {
		return Config{}, setupError("conflicting or invalid kubeconfig credentials", "Give the kubeconfig user exactly one of token, tokenFile or a client certificate and key.")
	}
	return result, nil
}

func validFederatedTokenPath(value string) bool {
	return value != "" && len(value) <= 4096 && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func decodeCredentialData(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > int(maxCredentialFileBytes*2) {
		return nil, setupError("credential data too large", "Shorten the base64 credential data, or mount it as a file instead.")
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) > int(maxCredentialFileBytes) {
		return nil, setupError("invalid credential data", "Set the credential data as standard base64 of the PEM or token bytes.")
	}
	return decoded, nil
}

// DecodeCAData decodes bounded base64 PEM data for direct connector configuration.
func DecodeCAData(value string) ([]byte, error) {
	return decodeCredentialData(value)
}
func resolveRelative(base, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(base, value)
}
func findContext(values []namedContext, name string) (kubeContext, bool) {
	for _, value := range values {
		if value.Name == name {
			return value.Context, true
		}
	}
	return kubeContext{}, false
}
func findCluster(values []namedCluster, name string) (kubeCluster, bool) {
	for _, value := range values {
		if value.Name == name {
			return value.Cluster, true
		}
	}
	return kubeCluster{}, false
}
func findUser(values []namedUser, name string) (kubeUser, bool) {
	for _, value := range values {
		if value.Name == name {
			return value.User, true
		}
	}
	return kubeUser{}, false
}
func hasDuplicateNames[T namedCluster | namedContext | namedUser](values []T) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		var name string
		switch entry := any(value).(type) {
		case namedCluster:
			name = entry.Name
		case namedContext:
			name = entry.Name
		case namedUser:
			name = entry.Name
		}
		if _, exists := seen[name]; exists {
			return true
		}
		seen[name] = struct{}{}
	}
	return false
}
func hasExplicitCredential(o AuthOptions) bool {
	return hasDirectCredential(o) || hasCloudCredential(o)
}
func hasDirectCredential(o AuthOptions) bool {
	return o.Token != "" || o.TokenFile != "" || o.CertificateFile != "" || o.KeyFile != "" || o.CertificateData != "" || o.KeyData != "" || hasKubeconfigCredential(o)
}
func hasClientCertificateCredential(o AuthOptions) bool {
	return o.CertificateFile != "" || o.KeyFile != "" || o.CertificateData != "" || o.KeyData != ""
}
func hasNonTokenCredential(o AuthOptions) bool {
	return hasClientCertificateCredential(o) || hasKubeconfigCredential(o) || hasCloudCredential(o)
}
func hasCloudCredential(o AuthOptions) bool {
	return hasEKSCredential(o) || hasAKSCredential(o) || o.GKECredentialsFile != ""
}
func hasKubeconfigCredential(o AuthOptions) bool {
	return o.KubeconfigPath != "" || o.KubeconfigContext != ""
}
func hasEKSCredential(o AuthOptions) bool {
	return o.EKSClusterName != "" || o.EKSRegion != "" || o.EKSRoleARN != "" || o.EKSProfile != ""
}
func hasAKSCredential(o AuthOptions) bool {
	return o.AKSCredentialMode != "" || o.AKSServerID != "" || o.AKSTenantID != "" || o.AKSClientID != "" || o.AKSClientSecret != "" || o.AKSFederatedTokenFile != "" || o.AKSEnvironment != ""
}
