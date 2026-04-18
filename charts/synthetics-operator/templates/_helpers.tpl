{{- define "synthetics-operator.name" -}}
synthetics-operator
{{- end -}}

{{- define "synthetics-operator.fullname" -}}
{{ include "synthetics-operator.name" . }}
{{- end -}}

{{/*
synthetics-operator.componentImage renders the image reference for one of
the operator's four components. Pass a dict:
    { "ctx": ., "image": .Values.controller.image }.
Precedence:
  1. .image.ref — full image ref override.
  2. {.image.repository}:{.image.tag}, where tag falls back to the chart
     AppVersion when left empty in values. Releases stamp Chart.yaml's
     appVersion from the git tag, so a user can `helm install --version 1.2.3`
     and get matching images for free.
*/}}
{{- define "synthetics-operator.componentImage" -}}
{{- if .image.ref -}}
{{ .image.ref }}
{{- else -}}
{{- $tag := default .ctx.Chart.AppVersion .image.tag -}}
{{ printf "%s:%s" .image.repository $tag }}
{{- end -}}
{{- end -}}

{{/*
synthetics-operator.runnerImage renders an image ref for a runner — test-
sidecar, k6-runner, playwright-runner. Each runner's Values block is a
single `image:` string (not repository+tag), matching how the operator
controller takes them on the CLI. Empty value falls back to the well-known
GHCR coordinate pinned to the chart AppVersion.
*/}}
{{- define "synthetics-operator.runnerImage" -}}
{{- if .override -}}
{{ .override }}
{{- else -}}
{{ printf "%s:%s" .defaultRepo .ctx.Chart.AppVersion }}
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
