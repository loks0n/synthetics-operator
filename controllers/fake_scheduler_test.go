package controllers

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

// fakeScheduler records Register/Unregister calls for use in unit tests.
// It satisfies the ProbeScheduler interface without starting any goroutines.
type fakeScheduler struct {
	mu         sync.Mutex
	active     map[types.NamespacedName]internalprobes.Job
	registered []types.NamespacedName
	removed    []types.NamespacedName
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{active: make(map[types.NamespacedName]internalprobes.Job)}
}

func (f *fakeScheduler) Register(job internalprobes.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[job.Key] = job
	f.registered = append(f.registered, job.Key)
}

func (f *fakeScheduler) Unregister(name types.NamespacedName) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, name)
	f.removed = append(f.removed, name)
}

func (f *fakeScheduler) isActive(name types.NamespacedName) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.active[name]
	return ok
}
