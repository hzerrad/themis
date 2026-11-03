{{- define "themis.name" -}}{{ .Release.Name }}{{- end }}

{{- define "themis.labels" -}}
app.kubernetes.io/part-of: themis
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "themis.selector" -}}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "themis.image" -}}
{{- $repo := .root.Values.image.repository -}}
{{- if $repo }}{{ $repo }}/{{ end }}themis-{{ .component }}:{{ .root.Values.image.tag }}
{{- end }}

{{- define "themis.databaseSecret" -}}
{{- .Values.database.existingSecret | default (printf "%s-database" .Release.Name) -}}
{{- end }}

{{- define "themis.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: [ALL]
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{/* OTLP export, when an endpoint is set; without one, nothing is traced. */}}
{{- define "themis.otlpEnv" -}}
{{- with .Values.telemetry.otlp.endpoint }}
- {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: {{ . | quote }}}
- {name: OTEL_EXPORTER_OTLP_PROTOCOL, value: {{ $.Values.telemetry.otlp.protocol | quote }}}
{{- end }}
{{- end }}

{{/* Annotations of the pre-upgrade hook that updates the CRDs. */}}
{{- define "themis.crdHook" -}}
helm.sh/hook: pre-upgrade
helm.sh/hook-weight: "{{ . }}"
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- end }}

{{/*
Whether the setup port is on. Values from before it existed, kept with
--reuse-values, have no setup key, so a missing key means on.
*/}}
{{- define "themis.setupEnabled" -}}
{{- $setup := .Values.setup | default dict -}}
{{- if not (and (hasKey $setup "enabled") (eq (toString $setup.enabled) "false")) }}true{{ end -}}
{{- end }}
