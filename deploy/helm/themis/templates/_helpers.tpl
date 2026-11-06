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
The API the investigator speaks: openai, the default, or anthropic, which has
its own address.
*/}}
{{- define "themis.modelProvider" -}}
{{- $provider := ((.Values.model | default dict).provider | default "openai" | toString | trim | lower) -}}
{{- if not (has $provider (list "openai" "anthropic")) -}}
{{- fail (printf "model.provider must be openai or anthropic, not %q" $provider) -}}
{{- end -}}
{{- $provider -}}
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
The alerting.alertmanager values, as JSON. Values from before they existed,
kept with --reuse-values, have none.
*/}}
{{- define "themis.alertmanagerValues" -}}
{{- (.Values.alerting | default dict).alertmanager | default dict | toJson -}}
{{- end }}

{{/*
Whether the core reads Alertmanager. A missing key means on, as above.
*/}}
{{- define "themis.alertmanagerEnabled" -}}
{{- $am := include "themis.alertmanagerValues" . | fromJson -}}
{{- if not (and (hasKey $am "enabled") (eq (toString $am.enabled) "false")) }}true{{ end -}}
{{- end }}

{{/*
A Service in the cluster, found at install with Helm's lookup: the first, by
namespace and name, that selects pods by the standard label
app.kubernetes.io/name set to .name and isn't headless, as
prometheus-community's prometheus and alertmanager charts and
kube-prometheus-stack make one for Alertmanager and Prometheus. Its port
named http or http-web, as those charts name it, or else .port. Empty when
there is none, and under helm template, Argo CD or Flux, where lookup finds
nothing. It uses the installer's access, never Themis's.
*/}}
{{- define "themis.findService" -}}
{{- $want := .name -}}
{{- $fallback := int .port -}}
{{- $found := "" -}}
{{- range (lookup "v1" "Service" "" "").items | default list -}}
{{- $selector := .spec.selector | default dict -}}
{{- if and (not $found) (eq (toString (get $selector "app.kubernetes.io/name")) $want) (ne (toString .spec.clusterIP) "None") -}}
{{- $port := 0 -}}
{{- range .spec.ports -}}
{{- if and (not $port) (has (toString .name) (list "http" "http-web")) }}{{ $port = int .port }}{{ end -}}
{{- end -}}
{{- range .spec.ports -}}
{{- if and (not $port) (eq (int .port) $fallback) }}{{ $port = $fallback }}{{ end -}}
{{- end -}}
{{- if $port }}{{ $found = printf "http://%s.%s.svc:%d" .metadata.name .metadata.namespace $port }}{{ end -}}
{{- end -}}
{{- end -}}
{{- $found -}}
{{- end }}

{{/*
An Alertmanager in the cluster, found at install.
*/}}
{{- define "themis.alertmanagerFound" -}}
{{- include "themis.findService" (dict "name" "alertmanager" "port" 9093) -}}
{{- end }}

{{/*
The Prometheus whose alerting rules the core checks, found at install, when
metrics.prometheus has no address.
*/}}
{{- define "themis.prometheusFound" -}}
{{- if not ((.Values.metrics | default dict).prometheus | default dict).address -}}
{{- include "themis.findService" (dict "name" "prometheus" "port" 9090) -}}
{{- end -}}
{{- end }}

{{/*
The alerting.rules values, as JSON. Values from before they existed, kept
with --reuse-values, have none.
*/}}
{{- define "themis.rulesValues" -}}
{{- (.Values.alerting | default dict).rules | default dict | toJson -}}
{{- end }}

{{/*
The labels of the PrometheusRule of the alerting rules: those set, or else
those the first Prometheus Operator Prometheus found selects rules by.
*/}}
{{- define "themis.ruleLabels" -}}
{{- $rules := include "themis.rulesValues" . | fromJson -}}
{{- $labels := $rules.labels | default dict -}}
{{- if not $labels -}}
{{- range (lookup "monitoring.coreos.com/v1" "Prometheus" "" "").items | default list -}}
{{- if not $labels -}}
{{- $labels = ((.spec | default dict).ruleSelector | default dict).matchLabels | default dict -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toJson $labels -}}
{{- end }}

{{/*
The address the core reads Alertmanager at: the one set, or the one found.
*/}}
{{- define "themis.alertmanagerAddress" -}}
{{- $am := include "themis.alertmanagerValues" . | fromJson -}}
{{- $am.address | default (include "themis.alertmanagerFound" .) -}}
{{- end }}

{{/*
The Secrets the core reads Alertmanager with, projected into one volume:
its token, the password of its basic auth, its CA and a client
certificate, each when set. They go only with the address set with them.
*/}}
{{- define "themis.alertmanagerSecrets" -}}
{{- $am := include "themis.alertmanagerValues" . | fromJson -}}
{{- $basic := $am.basicAuth | default dict -}}
{{- $tls := $am.tls | default dict -}}
{{- if include "themis.alertmanagerEnabled" . }}
{{- if and (or $am.tokenSecret $basic.passwordSecret $tls.caSecret $tls.clientCertSecret $tls.insecureSkipVerify) (not $am.address) }}
{{- fail "alerting.alertmanager.address is required with its token, basic auth or TLS settings. Themis sends them only to the address set with them." }}
{{- end }}
{{- if and $am.tokenSecret $basic.passwordSecret }}
{{- fail "Set alerting.alertmanager.tokenSecret or alerting.alertmanager.basicAuth, not both." }}
{{- end }}
{{- with $am.tokenSecret }}
- secret: {name: {{ . }}, items: [{key: token, path: token}]}
{{- end }}
{{- with $basic.passwordSecret }}
- secret: {name: {{ . }}, items: [{key: password, path: password}]}
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

{{/*
The username of the admin account the install configures, or nothing. A
password in values is refused: Git and Helm's release history keep values.
*/}}
{{- define "themis.adminAccount" -}}
{{- $initial := .Values.initialAdmin | default dict -}}
{{- $account := $initial.account | default dict -}}
{{- range $key := list "password" "passwordHash" }}
{{- if or (hasKey $initial $key) (hasKey $account $key) }}
{{- fail "Refused. initialAdmin holds a password. Values are kept in Git and in Helm's release history. Put the password or its hash in a Secret, and set initialAdmin.account.existingSecret." }}
{{- end }}
{{- end }}
{{- with $account.username }}
{{- $_ := required "initialAdmin.account.existingSecret is required with initialAdmin.account.username. Name the Secret that holds the password or its hash." $account.existingSecret }}
{{- . }}
{{- end }}
{{- end }}

{{/*
The Secret holding the initial admin password: the one the chart generates,
or one the install provides, as GitOps tools need.
*/}}
{{- define "themis.initialAdminSecret" -}}
{{- (.Values.initialAdmin | default dict).existingSecret | default (printf "%s-initial-admin" .Release.Name) -}}
{{- end }}
