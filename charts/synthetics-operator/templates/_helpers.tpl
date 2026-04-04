{{- define "synthetics-operator.name" -}}
synthetics-operator
{{- end -}}

{{- define "synthetics-operator.fullname" -}}
{{ include "synthetics-operator.name" . }}
{{- end -}}

{{- define "synthetics-operator.image" -}}
{{- if .Values.image.ref -}}
{{ .Values.image.ref }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end -}}
{{- end -}}

{{- define "synthetics-operator.webhookServiceName" -}}
{{ .Values.webhook.serviceName }}
{{- end -}}

{{- define "synthetics-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{ .Values.serviceAccount.name }}
{{- else -}}
{{ include "synthetics-operator.fullname" . }}
{{- end -}}
{{- end -}}
