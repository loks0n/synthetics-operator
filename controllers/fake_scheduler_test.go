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
	registered []types.NamespacedName
	removed    []types.NamespacedName
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{active: make(map[types.NamespacedName]time.Duration)}
}

func (f *fakeScheduler) Register(key types.NamespacedName, _ results.Kind, interval time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[key] = interval
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
