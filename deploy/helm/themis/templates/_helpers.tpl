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
