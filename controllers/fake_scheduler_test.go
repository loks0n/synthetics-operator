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
	active     map[fakeSchedulerKey]time.Duration
	specs      map[fakeSchedulerKey]results.SpecUpdate
	registered []fakeSchedulerKey
	removed    []fakeSchedulerKey
}

type fakeSchedulerKey struct {
	kind results.Kind
	name types.NamespacedName
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		active: make(map[fakeSchedulerKey]time.Duration),
		specs:  make(map[fakeSchedulerKey]results.SpecUpdate),
	}
}

func (f *fakeScheduler) Register(spec results.SpecUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeSchedulerKey{kind: spec.Kind, name: types.NamespacedName{Namespace: spec.Namespace, Name: spec.Name}}
	f.active[key] = time.Duration(spec.IntervalMs) * time.Millisecond
	f.specs[key] = spec
	f.registered = append(f.registered, key)
}

func (f *fakeScheduler) Unregister(kind results.Kind, name types.NamespacedName) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeSchedulerKey{kind: kind, name: name}
	delete(f.active, key)
	f.removed = append(f.removed, key)
}

func (f *fakeScheduler) isActive(kind results.Kind, name types.NamespacedName) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.active[fakeSchedulerKey{kind: kind, name: name}]
	return ok
}
