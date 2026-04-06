# -*- mode: Python -*-

# Restrict to the local dev cluster — prevents accidental deploys to production.
allow_k8s_contexts('kind-synthetics-dev')

IMAGE = 'ko.local/synthetics-operator'

# Build with ko into the local Docker daemon, then tag with the ref Tilt expects.
# Tilt finds IMAGE in the rendered Helm YAML and substitutes the live-built ref.
custom_build(
    IMAGE,
    'make ko-build-local && docker tag ' + IMAGE + ' $EXPECTED_REF',
    deps=[
        'main.go',
        'api/',
        'controllers/',
        'internal/',
        'go.mod',
        'go.sum',
    ],
)

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

# Forward the metrics endpoint so you can curl http://localhost:8080/metrics.
# objects pulls the namespace (applied separately from the Helm chart) into this resource group.
k8s_resource(
    'synthetics-operator',
    port_forwards=['8080:8080'],
    labels=['operator'],
    objects=[
        'synthetics-system:namespace',
        'httpprobes.synthetics.dev:customresourcedefinition',
        'synthetics-operator:clusterrole',
        'synthetics-operator:clusterrolebinding',
        'synthetics-operator:serviceaccount',
        'synthetics-operator-mutating-webhook-configuration:mutatingwebhookconfiguration',
        'synthetics-operator-validating-webhook-configuration:validatingwebhookconfiguration',
    ],
)
