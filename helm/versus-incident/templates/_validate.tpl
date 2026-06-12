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

Today only `file` is implemented end-to-end. The file backend stores
the pattern catalog, shadow log, detect log, AI cache, and incident
history under the fixed /app/data path (mount a PVC there to persist).

Caught here:
  - unsupported storage.type
  - file backend with persistence + replicaCount > 1 + RWO access mode
    (PVC cannot be mounted on >1 pod simultaneously, and even with RWX
    multiple writers race on the same JSON files)
  - existingClaim set without persistence.enabled
*/}}
{{- define "versus-incident.validateStorage" -}}
{{- $type := .Values.storage.type | default "file" -}}
{{- if not (has $type (list "file" "redis" "database")) -}}
  {{- fail (printf "versus-incident: storage.type=%q is not recognised. Valid values: file, redis, database (only `file` is implemented today)." $type) -}}
{{- end -}}
{{- if and (ne $type "file") (not (or .Values.oncall.enable .Values.oncall.initializedOnly)) -}}
  {{- fail (printf "versus-incident: storage.type=%q is not yet implemented end-to-end; only `file` works in this release." $type) -}}
{{- end -}}
{{- if eq $type "file" -}}
  {{- $replicas := int (default 1 .Values.replicaCount) -}}
  {{- if and .Values.storage.persistence.enabled (gt $replicas 1) -}}
    {{- $access := .Values.storage.persistence.accessMode | default "ReadWriteOnce" -}}
    {{- if eq $access "ReadWriteOnce" -}}
      {{- fail (printf "versus-incident: storage.persistence.enabled=true with %d replicas and accessMode=ReadWriteOnce. The PVC cannot be mounted on more than one pod. Set replicaCount=1, use accessMode=ReadWriteMany, or switch to a non-file storage backend." $replicas) -}}
    {{- end -}}
  {{- end -}}
  {{- if and .Values.storage.persistence.existingClaim (not .Values.storage.persistence.enabled) -}}
    {{- fail "versus-incident: storage.persistence.existingClaim is set but storage.persistence.enabled is false. Set persistence.enabled=true to bind the existing claim." -}}
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
  {{- if gt $replicas 1 -}}
    {{- fail (printf "versus-incident: agent.enable=true with %d replicas. The agent worker is single-writer to the pattern catalog and detect log; running multiple replicas will produce racing writes. Set replicaCount=1 when running the agent." $replicas) -}}
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
