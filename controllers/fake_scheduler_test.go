package controllers

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// fakeScheduler records Register/Unregister calls. It satisfies the
// ProbeScheduler interface without starting any goroutines.
type fakeScheduler struct {
	mu         sync.Mutex
	active     map[types.NamespacedName]time.Duration
	specs      map[types.NamespacedName]results.SpecUpdate
	registered []types.NamespacedName
	removed    []types.NamespacedName
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		active: make(map[types.NamespacedName]time.Duration),
		specs:  make(map[types.NamespacedName]results.SpecUpdate),
	}
}

func (f *fakeScheduler) Register(key types.NamespacedName, spec results.SpecUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[key] = time.Duration(spec.IntervalMs) * time.Millisecond
	f.specs[key] = spec
	f.registered = append(f.registered, key)
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
