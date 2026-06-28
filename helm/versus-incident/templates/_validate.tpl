{{/*
Validate Redis configuration.

Redis is required only when on-call is enabled. When required, the user must
choose exactly one of:
  - bundled Redis  (redis.enabled=true)
  - external Redis (redis.enabled=false AND externalRedis.host non-empty)

Common misconfigurations caught here:
  - on-call enabled but no Redis selected at all
  - redis.enabled=true alongside a non-empty externalRedis.host
    (the externalRedis.* block would be silently ignored — see issue #100)
*/}}
{{- define "versus-incident.validateRedis" -}}
{{- $oncallNeeded := or .Values.oncall.enable .Values.oncall.initializedOnly -}}
{{- if $oncallNeeded -}}
  {{- if and .Values.redis.enabled .Values.externalRedis.host -}}
    {{- fail (printf "versus-incident: ambiguous Redis configuration. redis.enabled=true and externalRedis.host=%q are both set, but externalRedis.* is only used when redis.enabled=false. Pick one mode (see helm/versus-incident/values.yaml)." .Values.externalRedis.host) -}}
  {{- end -}}
  {{- if and (not .Values.redis.enabled) (not .Values.externalRedis.host) -}}
    {{- fail "versus-incident: on-call is enabled but no Redis is configured. Either set redis.enabled=true to deploy bundled Redis, or set externalRedis.host to point at an existing Redis." -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Validate storage configuration.

Two backends are supported end-to-end:
  file     — local filesystem under /app/data; single-replica only.
  postgres — durable, shared, multi-writer; REQUIRED for HA (>1 replica).

Caught here:
  - unsupported storage.type
  - postgres without a DSN (no storage.postgres.dsn / dsnExistingSecret)
  - file backend with persistence + replicaCount > 1 + RWO access mode
    (PVC cannot be mounted on >1 pod simultaneously, and even with RWX
    multiple writers race on the same JSON files)
  - existingClaim set without persistence.enabled
*/}}
{{- define "versus-incident.validateStorage" -}}
{{- $type := include "versus-incident.storageType" . -}}
{{- if not (has $type (list "file" "postgres")) -}}
  {{- fail (printf "versus-incident: storage.type=%q is not recognised. Valid values: file, postgres." $type) -}}
{{- end -}}
{{- if eq $type "postgres" -}}
  {{- if and (not .Values.storage.postgres.dsn) (not .Values.storage.postgres.dsnExistingSecret) -}}
    {{- fail "versus-incident: storage.type=postgres requires a DSN. Set storage.postgres.dsn (e.g. postgres://user:pass@host:5432/versus?sslmode=require) or storage.postgres.dsnExistingSecret to reference a Secret." -}}
  {{- end -}}
{{- end -}}
{{- if eq $type "file" -}}
  {{- $replicas := int (default 1 .Values.replicaCount) -}}
  {{- if and .Values.storage.persistence.enabled (gt $replicas 1) -}}
    {{- $access := .Values.storage.persistence.accessMode | default "ReadWriteOnce" -}}
    {{- if eq $access "ReadWriteOnce" -}}
      {{- fail (printf "versus-incident: storage.persistence.enabled=true with %d replicas and accessMode=ReadWriteOnce. The PVC cannot be mounted on more than one pod. Set replicaCount=1, use accessMode=ReadWriteMany, or switch to storage.type=postgres (ha.enabled=true)." $replicas) -}}
    {{- end -}}
  {{- end -}}
  {{- if and .Values.storage.persistence.existingClaim (not .Values.storage.persistence.enabled) -}}
    {{- fail "versus-incident: storage.persistence.existingClaim is set but storage.persistence.enabled is false. Set persistence.enabled=true to bind the existing claim." -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Validate HA / multi-instance (epic X9) configuration.

HA is an ENTERPRISE capability: it runs the enterprise image with a LICENSE_KEY,
renders a StatefulSet whose stable POD_NAME ordinals supply each replica's
instance index, sets INSTANCE_COUNT from replicaCount, and shares ONE Postgres
across all replicas (the binary log.Fatals on file + INSTANCE_COUNT>1).

Caught here:
  - ha.enabled without an enterprise license (licenseKey / existingSecret)
  - replicaCount < 1
The postgres-backend + DSN requirement is enforced by validateStorage above
(storageType is DERIVED to postgres whenever ha.enabled).
*/}}
{{- define "versus-incident.validateHA" -}}
{{- if .Values.ha.enabled -}}
  {{- if and (not .Values.enterprise.licenseKey) (not .Values.enterprise.existingSecret) -}}
    {{- fail "versus-incident: ha.enabled=true is an ENTERPRISE capability and requires a license. Set enterprise.licenseKey (e.g. --set enterprise.licenseKey=...) or enterprise.existingSecret to reference a Secret that holds it." -}}
  {{- end -}}
  {{- $replicas := int (default 1 .Values.replicaCount) -}}
  {{- if lt $replicas 1 -}}
    {{- fail (printf "versus-incident: replicaCount must be >= 1, got %d." $replicas) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Validate agent configuration.

Caught here:
  - unrecognised agent.mode
  - agent.enable=true with replicaCount > 1 (the agent worker is single-
    writer to the catalog/detect log; multiple replicas race)
  - agent.ai.enable=true with empty agent.ai.apiKey
  - detect mode without agent.ai.enable=true (silently dry-runs — surface it)
*/}}
{{- define "versus-incident.validateAgent" -}}
{{- if .Values.agent.enable -}}
  {{- $mode := .Values.agent.mode | default "training" -}}
  {{- if not (has $mode (list "training" "shadow" "detect")) -}}
    {{- fail (printf "versus-incident: agent.mode=%q is not recognised. Valid values: training, shadow, detect." $mode) -}}
  {{- end -}}
  {{- $replicas := int (default 1 .Values.replicaCount) -}}
  {{- if and (gt $replicas 1) (not .Values.ha.enabled) -}}
    {{- fail (printf "versus-incident: agent.enable=true with %d replicas. The OSS agent worker is single-writer to the pattern catalog and detect log; running multiple replicas will produce racing writes. Set replicaCount=1, or enable ha.enabled=true (enterprise) which partitions signal sources across replicas by hash-ownership." $replicas) -}}
  {{- end -}}
  {{- if .Values.agent.ai.enable -}}
    {{- if not .Values.agent.ai.apiKey -}}
      {{- fail "versus-incident: agent.ai.enable=true but agent.ai.apiKey is empty. Set the OpenAI API key (preferably via --set agent.ai.apiKey or an external secret)." -}}
    {{- end -}}
  {{- end -}}
  {{- if and (eq $mode "detect") (not .Values.agent.ai.enable) -}}
    {{/* Not fatal — detect mode without AI is a valid dry-run. Just a hint. */}}
  {{- end -}}
{{- end -}}
{{- end -}}
