package main

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// nodeSelectorFlag collects `key=value` label pairs from the command line into
// the cluster-wide default nodeSelector for CronJob-backed test runner pods.
// Repeat the flag or comma-separate the pairs — a label value cannot contain a
// comma, so splitting on one is unambiguous.
//
// Pairs go through the same validation as a test's own spec.runner.nodeSelector
// so a malformed selector fails the process at boot rather than producing
// CronJobs the API server rejects.
type nodeSelectorFlag map[string]string

func (f nodeSelectorFlag) String() string {
	pairs := make([]string, 0, len(f))
	for key, value := range f {
		pairs = append(pairs, key+"="+value)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

func (f nodeSelectorFlag) Set(value string) error {
	for pair := range strings.SplitSeq(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, labelValue, found := strings.Cut(pair, "=")
		if !found {
			return fmt.Errorf("%q is not a key=value pair", pair)
		}
		selector := map[string]string{key: labelValue}
		if errs := syntheticsv1alpha1.ValidateRunnerNodeSelector(selector, field.NewPath("runner-node-selector")); len(errs) > 0 {
			return errs.ToAggregate()
		}
		f[key] = labelValue
	}
	return nil
}
