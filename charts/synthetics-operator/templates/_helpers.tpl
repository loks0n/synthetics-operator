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

{{- define "synthetics-operator.webhookServiceAccountName" -}}
{{ include "synthetics-operator.fullname" . }}-webhook
{{- end -}}

{{/*
synthetics-operator.natsURL resolves the NATS URL the operator and test
sidecars should use. Precedence:
  1. nats.externalUrl, if set — points at an externally managed NATS.
  2. Internal NATS (when nats.enabled: true) — FQDN so the URL works
     when forwarded into pods running in user namespaces.
  3. Empty, if neither — CronJob-backed tests won't publish results.
*/}}
{{- define "synthetics-operator.natsURL" -}}
{{- if .Values.nats.externalUrl -}}
{{ .Values.nats.externalUrl }}
{{- else if .Values.nats.enabled -}}
nats://{{ include "synthetics-operator.fullname" . }}-nats.{{ .Release.Namespace }}.svc:4222
{{- end -}}
{{- end -}}
