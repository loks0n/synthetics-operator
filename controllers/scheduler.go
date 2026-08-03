package controllers

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// ProbeScheduler is the scheduling surface reconcilers depend on. The
// concrete scheduler publishes a ProbeJob to NATS on each tick; a
// prober pulls the job off a queue group and executes.
type ProbeScheduler interface {
	Register(key types.NamespacedName, spec results.SpecUpdate)
	Unregister(name types.NamespacedName)
}
