{{- define "versus-incident.connectorAuth" -}}
{{- $auth := .auth -}}
{{- $mode := $auth.mode | default "" -}}
mode: {{ $mode | quote }}
{{- if eq $mode "token" }}
token: {{ printf "${%s_TOKEN}" .prefix | quote }}
{{- else if eq $mode "token_file" }}
token_file: {{ $auth.tokenFile | default "" | quote }}
{{- else if eq $mode "client_certificate" }}
client_certificate:
  certificate_file: {{ ($auth.clientCertificate | default dict).certificateFile | default "" | quote }}
  key_file: {{ ($auth.clientCertificate | default dict).keyFile | default "" | quote }}
  certificate_data: {{ printf "${%s_CLIENT_CERTIFICATE_DATA}" .prefix | quote }}
  key_data: {{ printf "${%s_CLIENT_KEY_DATA}" .prefix | quote }}
{{- else if eq $mode "kubeconfig" }}
kubeconfig:
  path: {{ ($auth.kubeconfig | default dict).path | default "" | quote }}
  context: {{ ($auth.kubeconfig | default dict).context | default "" | quote }}
{{- else if eq $mode "eks" }}
eks:
  cluster_name: {{ ($auth.eks | default dict).clusterName | default "" | quote }}
  region: {{ ($auth.eks | default dict).region | default "" | quote }}
  role_arn: {{ ($auth.eks | default dict).roleARN | default "" | quote }}
  profile: {{ ($auth.eks | default dict).profile | default "" | quote }}
{{- else if eq $mode "aks" }}
{{- $aks := $auth.aks | default dict }}
{{- $credentialMode := $aks.credentialMode | default "" }}
aks:
  credential_mode: {{ $credentialMode | quote }}
  server_id: {{ $aks.serverID | default "" | quote }}
  client_id: {{ $aks.clientID | default "" | quote }}
  {{- if or (eq $credentialMode "workload_identity") (eq $credentialMode "client_secret") }}
  tenant_id: {{ $aks.tenantID | default "" | quote }}
  {{- end }}
  {{- if eq $credentialMode "client_secret" }}
  client_secret: {{ printf "${%s_AKS_CLIENT_SECRET}" .prefix | quote }}
  {{- else if eq $credentialMode "workload_identity" }}
  federated_token_file: {{ $aks.federatedTokenFile | default "" | quote }}
  {{- end }}
  environment: {{ $aks.environment | default "" | quote }}
{{- else if eq $mode "gke" }}
gke:
  credentials_file: {{ ($auth.gke | default dict).credentialsFile | default "" | quote }}
{{- end }}
{{- end -}}