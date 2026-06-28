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
