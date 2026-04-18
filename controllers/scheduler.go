package controllers

import (
	"k8s.io/apimachinery/pkg/types"

	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

// ProbeScheduler is the scheduling interface controllers depend on.
type ProbeScheduler interface {
	Register(job internalprobes.Job)
	Unregister(name types.NamespacedName)
}
