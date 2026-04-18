# -*- mode: Python -*-

load('ext://helm_resource', 'helm_resource', 'helm_repo')

# Restrict to the local dev cluster — prevents accidental deploys to production.
allow_k8s_contexts('kind-synthetics-dev')

COMPONENTS = [
    ('controller', 'ko.local/synthetics-operator-controller'),
    ('webhook', 'ko.local/synthetics-operator-webhook'),
    ('prober', 'ko.local/synthetics-operator-prober'),
    ('metrics', 'ko.local/synthetics-operator-metrics'),
]

# Build each cmd/ binary via ko into the local Docker daemon, then tag with
# the ref Tilt expects. Tilt finds each IMAGE in the rendered Helm YAML and
# substitutes the live-built ref.
SHARED_DEPS = [
    'api/',
    'controllers/',
    'internal/',
    'go.mod',
    'go.sum',
]

for name, image in COMPONENTS:
    custom_build(
        image,
        'make ko-build-%s-local && docker tag %s $EXPECTED_REF' % (name, image),
        deps=SHARED_DEPS + ['cmd/' + name + '/'],
    )

# ── Operator ─────────────────────────────────────────────────────────────────

# The chart does not create a namespace.
k8s_yaml(blob('''
apiVersion: v1
kind: Namespace
metadata:
  name: synthetics-system
'''))

# Operator — deployed via Helm with dev overrides.
k8s_yaml(helm(
    'charts/synthetics-operator',
    name='synthetics-operator',
    namespace='synthetics-system',
    values=['hack/dev-values.yaml'],
))

k8s_resource(
    'synthetics-operator-controller',
    labels=['operator'],
    objects=[
        'synthetics-system:namespace',
        'httpprobes.synthetics.dev:customresourcedefinition',
        'dnsprobes.synthetics.dev:customresourcedefinition',
        'k6tests.synthetics.dev:customresourcedefinition',
        'playwrighttests.synthetics.dev:customresourcedefinition',
        'synthetics-operator-controller:clusterrole',
        'synthetics-operator-controller:clusterrolebinding',
        'synthetics-operator-controller:serviceaccount',
        'synthetics-operator-mutating-webhook-configuration:mutatingwebhookconfiguration',
        'synthetics-operator-validating-webhook-configuration:validatingwebhookconfiguration',
    ],
)

k8s_resource(
    'synthetics-operator-webhook',
    labels=['operator'],
    objects=[
        'synthetics-operator-webhook:clusterrole',
        'synthetics-operator-webhook:clusterrolebinding',
        'synthetics-operator-webhook:serviceaccount',
        'synthetics-operator-webhook:poddisruptionbudget',
    ],
)

k8s_resource(
    'synthetics-operator-prober',
    labels=['operator'],
    objects=[
        'synthetics-operator-prober:serviceaccount',
    ],
)

k8s_resource(
    'synthetics-operator-metrics',
    port_forwards=['8080:8080'],
    labels=['operator'],
    objects=[
        'synthetics-operator-metrics:serviceaccount',
    ],
)

# ── Metrics server (enables kubectl top / k9s resource usage) ────────────────

helm_repo('metrics-server-repo', 'https://kubernetes-sigs.github.io/metrics-server/', labels=['operator'])

helm_resource(
    'metrics-server',
    'metrics-server-repo/metrics-server',
    flags=[
        '--namespace=kube-system',
        '--set=args={--kubelet-insecure-tls}',
    ],
    resource_deps=['metrics-server-repo'],
    labels=['operator'],
)

# ── Monitoring stack (Prometheus + Grafana) ───────────────────────────────────

MONITORING_NAMESPACE = 'monitoring'
HACK_DIR = config.main_dir + '/hack'

k8s_yaml(blob('''
apiVersion: v1
kind: Namespace
metadata:
  name: monitoring
'''))

helm_repo('prometheus-community', 'https://prometheus-community.github.io/helm-charts', labels=['monitoring'])
helm_repo('grafana-repo', 'https://grafana.github.io/helm-charts', labels=['monitoring'])
helm_repo('open-telemetry', 'https://open-telemetry.github.io/opentelemetry-helm-charts', labels=['monitoring'])

helm_resource(
    'otel-collector',
    'open-telemetry/opentelemetry-collector',
    flags=[
        '--version=0.96.0',
        '--create-namespace',
        '--values=' + HACK_DIR + '/otel-collector-values.yaml',
    ],
    namespace=MONITORING_NAMESPACE,
    resource_deps=['open-telemetry'],
    labels=['monitoring'],
)

helm_resource(
    'prometheus',
    'prometheus-community/prometheus',
    flags=[
        '--version=25.8.0',
        '--create-namespace',
        '--values=' + HACK_DIR + '/prometheus-values.yaml',
    ],
    namespace=MONITORING_NAMESPACE,
    resource_deps=['prometheus-community', 'otel-collector'],
    labels=['monitoring'],
    port_forwards=['9090:9090'],
    links=[link('http://localhost:9090', 'Prometheus')],
)

helm_resource(
    'grafana',
    'grafana-repo/grafana',
    flags=[
        '--version=8.5.1',
        '--create-namespace',
        '--values=' + HACK_DIR + '/grafana-values.yaml',
    ],
    namespace=MONITORING_NAMESPACE,
    resource_deps=['grafana-repo', 'prometheus'],
    labels=['monitoring'],
    port_forwards=['3000:3000'],
    links=[link('http://localhost:3000', 'Grafana')],
)

# Dashboard ConfigMaps — Grafana sidecar auto-provisions any ConfigMap labelled grafana_dashboard=1.
# Regenerate after editing dashboards/: make dashboard-configmaps
# (alerts/synthetics-rules.yaml is a PrometheusRule for environments with the Prometheus operator)
k8s_yaml('hack/dashboard-configmaps.yaml')

k8s_resource(
    new_name='monitoring-config',
    objects=[
        'monitoring:namespace',
        'synthetics-overview-dashboard:configmap:monitoring',
        'synthetics-http-dashboard:configmap:monitoring',
        'synthetics-dns-dashboard:configmap:monitoring',
        'synthetics-playwright-dashboard:configmap:monitoring',
        'synthetics-k6-dashboard:configmap:monitoring',
    ],
    labels=['monitoring'],
)
