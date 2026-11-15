{{/*
Expand the name of the chart.
*/}}
{{- define "versus-incident.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "versus-incident.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "versus-incident.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "versus-incident.labels" -}}
helm.sh/chart: {{ include "versus-incident.chart" . }}
{{ include "versus-incident.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "versus-incident.selectorLabels" -}}
app.kubernetes.io/name: {{ include "versus-incident.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "versus-incident.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "versus-incident.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image tag. An explicit image.tag is used verbatim (so enterprise
tags like "dev" work); empty defaults to v<appVersion> (the OSS release tag
convention, unchanged).
*/}}
{{- define "versus-incident.imageTag" -}}
{{- if .Values.image.tag -}}
{{- .Values.image.tag -}}
{{- else -}}
v{{ .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{/*
Effective storage backend. HA (ha.enabled) DERIVES the Postgres backend: all
replicas share ONE multi-writer Postgres, and the binary refuses the single-
node `file` backend with INSTANCE_COUNT>1. Single-instance honours
storage.type (default file).
*/}}
{{- define "versus-incident.storageType" -}}
{{- if .Values.ha.enabled -}}
postgres
{{- else -}}
{{- .Values.storage.type | default "file" -}}
{{- end -}}
{{- end -}}

{{/*
Headless Service name for the StatefulSet (stable per-pod DNS).
*/}}
{{- define "versus-incident.headlessServiceName" -}}
{{- default (printf "%s-headless" (include "versus-incident.fullname" .)) .Values.ha.headlessServiceName -}}
{{- end -}}

{{/*
License Secret reference (enterprise). Prefer an existing Secret; otherwise the
chart-managed Secret holds the inline enterprise.licenseKey under "license_key".
*/}}
{{- define "versus-incident.licenseSecretName" -}}
{{- if .Values.enterprise.existingSecret -}}
{{- .Values.enterprise.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "versus-incident.fullname" .) -}}
{{- end -}}
{{- end -}}
{{- define "versus-incident.licenseSecretKey" -}}
{{- if .Values.enterprise.existingSecret -}}
{{- .Values.enterprise.existingSecretKey | default "license_key" -}}
{{- else -}}
license_key
{{- end -}}
{{- end -}}

{{/*
Postgres DSN Secret reference. Prefer an existing Secret; otherwise the chart-
managed Secret holds the inline storage.postgres.dsn under "postgres_dsn".
*/}}
{{- define "versus-incident.postgresSecretName" -}}
{{- if .Values.storage.postgres.dsnExistingSecret -}}
{{- .Values.storage.postgres.dsnExistingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "versus-incident.fullname" .) -}}
{{- end -}}
{{- end -}}
{{- define "versus-incident.postgresSecretKey" -}}
{{- if .Values.storage.postgres.dsnExistingSecret -}}
{{- .Values.storage.postgres.dsnExistingSecretKey | default "postgres_dsn" -}}
{{- else -}}
postgres_dsn
{{- end -}}
{{- end -}}

{{/*
Optional BYOK master key (VERSUS_ENTERPRISE_SECRET_KEY) Secret reference.
*/}}
{{- define "versus-incident.enterpriseSecretKeyName" -}}
{{- if .Values.enterprise.secretKeyExistingSecret -}}
{{- .Values.enterprise.secretKeyExistingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "versus-incident.fullname" .) -}}
{{- end -}}
{{- end -}}
{{- define "versus-incident.enterpriseSecretKeyKey" -}}
{{- if .Values.enterprise.secretKeyExistingSecret -}}
{{- .Values.enterprise.secretKeyExistingSecretKey | default "versus_enterprise_secret_key" -}}
{{- else -}}
versus_enterprise_secret_key
{{- end -}}
{{- end -}}

{{/*
"true" when the Kubernetes connector is on. connectors.kubernetes.enabled
defaults to false; true (or the string "true") turns it on.
*/}}
{{- define "versus-incident.kubernetesConnectorEnabled" -}}
{{- $kubernetes := (.Values.connectors | default dict).kubernetes | default dict -}}
{{- $enabled := $kubernetes.enabled -}}
{{- if or (and (kindIs "bool" $enabled) $enabled) (and (kindIs "string" $enabled) (eq ($enabled | trim | lower) "true")) -}}
true
{{- end -}}
{{- end -}}

{{/*
"true" when the enabled connector reads the cluster it runs in with the pod
ServiceAccount (auth.mode=in_cluster, or any multi-cluster entry using it).
*/}}
{{- define "versus-incident.kubernetesInCluster" -}}
{{- if include "versus-incident.kubernetesConnectorEnabled" . -}}
{{- $kubernetes := (.Values.connectors | default dict).kubernetes | default dict -}}
{{- if $kubernetes.multiple -}}
{{- range $cluster := ($kubernetes.clusters | default list) -}}
{{- if eq ((($cluster | default dict).auth | default dict).mode | default "") "in_cluster" -}}true{{- end -}}
{{- end -}}
{{- else if eq (($kubernetes.auth | default dict).mode | default "") "in_cluster" -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
"true" when the chart creates the Kubernetes reader ClusterRole. An empty
kubernetesReaderRBAC.enabled follows the connector: on for in_cluster. An
explicit true or false wins. external=true means the user manages the
connector's RBAC, so the chart creates nothing.
*/}}
{{- define "versus-incident.kubernetesReaderRBACEnabled" -}}
{{- $rbac := .Values.kubernetesReaderRBAC | default dict -}}
{{- if not $rbac.external -}}
{{- $enabled := include "versus-incident.kubernetesReaderRBACSetting" . -}}
{{- if $enabled -}}
{{- if eq $enabled "true" -}}true{{- end -}}
{{- else if include "versus-incident.kubernetesInCluster" . -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
kubernetesReaderRBAC.enabled as "true", "false" or "" (empty: follow the
connector). Booleans and the strings "true"/"false" (any case) are accepted;
anything else fails, so a quoted "false" can never be read as on.
*/}}
{{- define "versus-incident.kubernetesReaderRBACSetting" -}}
{{- $value := (.Values.kubernetesReaderRBAC | default dict).enabled -}}
{{- if kindIs "bool" $value -}}
{{- ternary "true" "false" $value -}}
{{- else if kindIs "string" $value -}}
{{- $text := $value | trim | lower -}}
{{- if has $text (list "true" "false") -}}
{{- $text -}}
{{- else if $text -}}
{{- fail (printf "versus-incident: kubernetesReaderRBAC.enabled must be true, false or empty, got %q." $value) -}}
{{- end -}}
{{- else if not (kindIs "invalid" $value) -}}
{{- fail (printf "versus-incident: kubernetesReaderRBAC.enabled must be true, false or empty, got a %s." (kindOf $value)) -}}
{{- end -}}
{{- end -}}

{{/*
Sign-in secret (GATEWAY_SECRET) reference. gatewaySecretExistingSecret points
at a Secret the user creates; otherwise the chart-managed Secret holds it.
*/}}
{{- define "versus-incident.gatewaySecretName" -}}
{{- if .Values.gatewaySecretExistingSecret -}}
{{- .Values.gatewaySecretExistingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "versus-incident.fullname" .) -}}
{{- end -}}
{{- end -}}
{{- define "versus-incident.gatewaySecretKey" -}}
{{- if .Values.gatewaySecretExistingSecret -}}
{{- .Values.gatewaySecretExistingSecretKey | default "gateway_secret" -}}
{{- else -}}
gateway_secret
{{- end -}}
{{- end -}}

{{/*
Base64 sign-in secret for the chart-managed Secret. A user-set gatewaySecret
wins. Otherwise the value already stored in the release's Secret is reused, so
upgrades keep the same secret; a first install generates 32 random bytes,
hex-encoded. The value is memoised on the root context so the Secret and the
pod checksum annotation see the same random value within one render.
*/}}
{{- define "versus-incident.gatewaySecretData" -}}
{{- if .Values.gatewaySecret -}}
{{- .Values.gatewaySecret | b64enc -}}
{{- else -}}
{{- if not (hasKey $.Values "__generatedGatewaySecret") -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-secrets" (include "versus-incident.fullname" .)) -}}
{{- $stored := "" -}}
{{- if $existing -}}
{{- $stored = index ($existing.data | default dict) "gateway_secret" | default "" -}}
{{- end -}}
{{- if not $stored -}}
{{- $stored = randAlphaNum 64 | sha256sum | trunc 64 | b64enc -}}
{{- end -}}
{{- $_ := set $.Values "__generatedGatewaySecret" $stored -}}
{{- end -}}
{{- index $.Values "__generatedGatewaySecret" -}}
{{- end -}}
{{- end -}}
