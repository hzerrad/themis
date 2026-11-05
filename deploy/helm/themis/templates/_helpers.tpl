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
Whether the investigator and its model are installed. Values from before
model.enabled existed, kept with --reuse-values, have no such key, so a
missing key means on.
*/}}
{{- define "themis.modelEnabled" -}}
{{- $model := .Values.model | default dict -}}
{{- if not (and (hasKey $model "enabled") (eq (toString $model.enabled) "false")) }}true{{ end -}}
{{- end }}

{{/*
Whether the setup port is on. A missing key means on, as above.
*/}}
{{- define "themis.setupEnabled" -}}
{{- $setup := .Values.setup | default dict -}}
{{- if not (and (hasKey $setup "enabled") (eq (toString $setup.enabled) "false")) }}true{{ end -}}
{{- end }}

{{/*
Whether the core reads the Kubernetes metrics API. A missing key means on, as
above.
*/}}
{{- define "themis.metricsAPIEnabled" -}}
{{- $api := (.Values.metrics | default dict).kubernetes | default dict -}}
{{- if not (and (hasKey $api "enabled") (eq (toString $api.enabled) "false")) }}true{{ end -}}
{{- end }}

{{/*
The Secrets the core reads Prometheus with, projected into one volume: its
token, its CA and a client certificate, each when set. Empty without any, or
without an address.
*/}}
{{- define "themis.prometheusSecrets" -}}
{{- $prom := (.Values.metrics | default dict).prometheus | default dict -}}
{{- $tls := $prom.tls | default dict -}}
{{- if $prom.address }}
{{- with $prom.tokenSecret }}
- secret: {name: {{ . }}, items: [{key: token, path: token}]}
{{- end }}
{{- with $tls.caSecret }}
- secret: {name: {{ . }}, items: [{key: ca.crt, path: ca.crt}]}
{{- end }}
{{- with $tls.clientCertSecret }}
- secret: {name: {{ . }}, items: [{key: tls.crt, path: tls.crt}, {key: tls.key, path: tls.key}]}
{{- end }}
{{- end }}
{{- end }}

{{/*
The Secret holding Themis's tokens and settings key: the one the chart
generates, or one the install provides, as GitOps tools need.
*/}}
{{- define "themis.tokensSecret" -}}
{{- (.Values.tokens | default dict).existingSecret | default (printf "%s-tokens" .Release.Name) -}}
{{- end }}
