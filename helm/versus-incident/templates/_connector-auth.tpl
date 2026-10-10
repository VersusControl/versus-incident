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

{{- define "versus-incident.clusterConnector" -}}
{{- $id := .clusterId | default .clusterID -}}
{{- $prefix := printf "KUBERNETES_CLUSTER_%s" ($id | upper | replace "-" "_") -}}
{{- $auth := .auth | default dict -}}
cluster_id: {{ $id | quote }}
display_name: {{ .displayName | default "" | quote }}
credential_id: {{ .credentialId | default .credentialID | default "" | quote }}
{{- if and (ne ($auth.mode | default "") "in_cluster") (ne ($auth.mode | default "") "kubeconfig") }}
endpoint: {{ .endpoint | default "" | quote }}
token_file: {{ .tokenFile | default "" | quote }}
ca_file: {{ .caFile | default "" | quote }}
ca_data: {{ printf "${%s_CA_DATA}" $prefix | quote }}
server_name: {{ .serverName | default "" | quote }}
{{- end }}
timeout: {{ .timeout | default "10s" | quote }}
discovery_ttl: {{ .discoveryTTL | default "5m" | quote }}
allow_loopback: {{ .allowLoopback | default false }}
allow_private_networks: {{ .allowPrivateNetworks | default false }}
endpoint_cidrs: {{ .endpointCIDRs | default list | toJson }}
auth:
  {{- include "versus-incident.connectorAuth" (dict "auth" $auth "prefix" $prefix) | nindent 2 }}
{{- $actions := .actions | default dict }}
actions:
  enable: {{ $actions.enable | default false }}
  max_replicas: {{ $actions.maxReplicas | default 20 }}
  timeout: {{ $actions.timeout | default "15s" | quote }}
  auth:
    {{- include "versus-incident.connectorAuth" (dict "auth" ($actions.auth | default dict) "prefix" (printf "%s_ACTOR" $prefix)) | nindent 4 }}
{{- end -}}

{{- define "versus-incident.validateClusters" -}}
{{- if or (lt (len (.clusters | default list)) 1) (gt (len (.clusters | default list)) 16) }}{{ fail "connectors.kubernetes.clusters must contain 1 to 16 entries" }}{{ end -}}
{{- if or .endpoint .tokenFile .caFile .caData .serverName .credentialID .credentialId .clusterId .allowLoopback .allowPrivateNetworks .endpointCIDRs (include "versus-incident.connectorHasValues" (.auth | default dict)) (include "versus-incident.connectorHasValues" ((.actions | default dict).auth | default dict)) (.actions | default dict).enable }}{{ fail "connectors.kubernetes: configure each cluster under clusters" }}{{ end -}}
{{- if or (and .clusterID (ne .clusterID "default")) (and .timeout (ne .timeout "10s")) (and .discoveryTTL (ne .discoveryTTL "5m")) (and (.actions | default dict).timeout (ne (.actions | default dict).timeout "15s")) (and (.actions | default dict).maxReplicas (ne (int (.actions | default dict).maxReplicas) 20)) }}{{ fail "connectors.kubernetes: configure each cluster under clusters" }}{{ end -}}
{{- $ids := dict -}}{{- $endpoints := dict -}}{{- $inCluster := false -}}
{{- range $index, $cluster := .clusters -}}
{{- $id := $cluster.clusterId | default $cluster.clusterID | default "" -}}
{{- if or (not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" $id)) (hasKey $ids $id) }}{{ fail (printf "connectors.kubernetes.clusters[%d]: invalid or duplicate cluster id" $index) }}{{ end -}}
{{- $_ := set $ids $id true -}}
{{- $endpoint := $cluster.endpoint | default "" | trim | trimSuffix "/" -}}
{{- if $endpoint -}}{{- if hasKey $endpoints $endpoint }}{{ fail (printf "connectors.kubernetes.clusters[%d]: duplicate endpoint" $index) }}{{ end -}}{{- $_ := set $endpoints $endpoint true -}}{{- end -}}
{{- if eq (($cluster.auth | default dict).mode | default "") "in_cluster" -}}{{- if $inCluster }}{{ fail (printf "connectors.kubernetes.clusters[%d]: duplicate in_cluster entry" $index) }}{{ end -}}{{- $inCluster = true -}}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "versus-incident.connectorHasValues" -}}
{{- range $key, $value := . -}}
{{- if kindIs "map" $value -}}
{{- include "versus-incident.connectorHasValues" $value -}}
{{- else if $value -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- define "versus-incident.connectorSecrets" -}}
{{- $auth := .auth | default dict -}}
{{- $actor := (.actions | default dict).auth | default dict -}}
{{- dict
"token" ($auth.token | default "" | b64enc)
"ca_data" (.caData | default "" | b64enc)
"client_certificate_data" (($auth.clientCertificate | default dict).certificateData | default "" | b64enc)
"client_key_data" (($auth.clientCertificate | default dict).keyData | default "" | b64enc)
"aks_client_secret" (($auth.aks | default dict).clientSecret | default "" | b64enc)
"actor_token" ($actor.token | default "" | b64enc)
"actor_client_certificate_data" (($actor.clientCertificate | default dict).certificateData | default "" | b64enc)
"actor_client_key_data" (($actor.clientCertificate | default dict).keyData | default "" | b64enc)
"actor_aks_client_secret" (($actor.aks | default dict).clientSecret | default "" | b64enc)
| toYaml -}}
{{- end -}}