{{- define "synthetics-operator.name" -}}
synthetics-operator
{{- end -}}

{{- define "synthetics-operator.fullname" -}}
{{ include "synthetics-operator.name" . }}
{{- end -}}

{{/*
synthetics-operator.componentImage renders the image reference for a
component. Pass a dict: { "ctx": ., "image": .Values.controller.image }.
Precedence: .image.ref > printf "%s:%s" .image.repository .image.tag.
*/}}
{{- define "synthetics-operator.componentImage" -}}
{{- if .image.ref -}}
{{ .image.ref }}
{{- else -}}
{{ printf "%s:%s" .image.repository .image.tag }}
{{- end -}}
{{- end -}}

{{- define "synthetics-operator.webhookServiceName" -}}
{{ .Values.webhook.serviceName }}
{{- end -}}

{{- define "synthetics-operator.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.name -}}
{{ .Values.controller.serviceAccount.name }}
{{- else -}}
{{ include "synthetics-operator.fullname" . }}-controller
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
