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
