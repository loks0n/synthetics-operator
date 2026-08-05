package controllers

import (
	"maps"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// runnerNodeSelector resolves the nodeSelector for a CronJob-backed test's
// runner pod. The operator-wide default (--runner-node-selector) is the base
// so that a cluster can pin every runner to a node pool once; a test's own
// spec.runner.nodeSelector merges over it key-by-key.
//
// Merging rather than replacing keeps the cluster default in force for tests
// that only mean to add a constraint — a test that overrides one key does not
// silently escape the pool it was meant to run in. Returns nil when neither
// side sets anything, leaving the pod spec field unset.
func runnerNodeSelector(defaults map[string]string, runner *syntheticsv1alpha1.RunnerSpec) map[string]string {
	var override map[string]string
	if runner != nil {
		override = runner.NodeSelector
	}
	if len(defaults) == 0 && len(override) == 0 {
		return nil
	}

	selector := make(map[string]string, len(defaults)+len(override))
	maps.Copy(selector, defaults)
	maps.Copy(selector, override)
	return selector
}
