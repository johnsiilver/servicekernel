/*
Package kernel implements a service kernel that manages modules and their interactions via message passing.
It allows for module registration, subscription to topics, and message publishing.

A note on topics: As of now it requires direct topic matching. However, this will likely be extended to support
various topic matching patterns in the future. Use of [], *, {}, ? \ will likely cause support issues in the future.
Simply use file like paths like /path/to/package/topic. You have been warned.
*/
package kernel

import (
	"errors"
	"fmt"

	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/generics/sets"
	"github.com/gostdlib/base/values/immutable"
	"github.com/gostdlib/base/values/immutable/unsafe"
)

// listener is a struct that holds a module and its handler for a specific topic.
type listener[T any] struct {
	m Module[T]
	h Handler[T]
}

// Kernel implements a service kernel.
type Kernel[T any] struct {
	mu sync.Mutex
	// moduleNames is a set of module names to ensure uniqueness.
	moduleNames sets.Set[string]
	modules     []Module[T]

	// note: at some point this probably should become a trie tree to allow for more complex topic matching.
	topics          sync.ShardedMap[string, immutable.Slice[listener[T]]]
	topicsEqualOnce sync.Once

	all sync.ShardedMap[string, listener[T]]

	started bool
}

// Start starts the kernel. This should be called after all modules are registered. If Start returns an error
// it should be considered a fatal error and the kernel should not be used further. If the context is cancelled
// it should stop all the modules that are running, if the modules are implemented correctly.
func (k *Kernel[T]) Start(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.started {
		return fmt.Errorf("kernel has already started")
	}

	k.topicsEqualOnce.Do(k.setupTopics)

	k.started = true

	for _, m := range k.modules {
		if err := m.Init(ctx, k); err != nil {
			return fmt.Errorf("failed to initialize module %s: %w", m.Name(), err)
		}
	}
	for _, m := range k.modules {
		if err := m.Start(ctx); err != nil {
			return fmt.Errorf("failed to start module %s: %w", m.Name(), err)
		}
	}
	return nil
}

// Register a module within the kernel. The module's name must be unique.
func (k *Kernel[T]) Register(m Module[T]) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.started {
		return fmt.Errorf("kernel has already started, cannot register new modules")
	}
	k.topicsEqualOnce.Do(k.setupTopics)

	if m == nil {
		return fmt.Errorf("module cannot be nil")
	}
	if m.Name() == "" {
		return fmt.Errorf("module name cannot be empty")
	}
	if k.moduleNames.Contains(m.Name()) {
		return fmt.Errorf("module with name %s already exists", m.Name())
	}
	k.moduleNames.Add(m.Name())
	k.modules = append(k.modules, m)
	return nil
}

// Registry will return a set of all registered modules/
// This should not be called until the kernel has been started or inside a Moduel's Start method.
func (k *Kernel[T]) Registry() sets.Set[string] {
	return k.moduleNames.Union(sets.Set[string]{})
}

// Subscribe allows a module to subscribe to a topic or set of topics. The module will receive messages published to that topic.
// This should not be called until the kernel has been started. Multiple modules can subscribe to the same topic.
func (k *Kernel[T]) Subscribe(topic string, m Module[T], h Handler[T]) {
	if topic == "" || m.Name() == "" || h == nil {
		return
	}
	k.topicsEqualOnce.Do(k.setupTopics)

	if topic == "*" {
		k.all.Set(m.Name(), listener[T]{m: m, h: h})
		return
	}

	old, ok := k.topics.Get(topic)
	if !ok {
		s := []listener[T]{{m: m, h: h}}
		v := immutable.NewSlice[listener[T]](s)
		if k.topics.CompareAndSwap(topic, old, v) {
			return
		}
		k.Subscribe(topic, m, h)
		return
	}

	for _, l := range old.All() {
		if l.m.Name() == m.Name() {
			// Module already subscribed to this topic, no need to add again.
			return
		}
	}

	s := make([]listener[T], old.Len()+1)
	copy(s, old.Copy())
	s[old.Len()] = listener[T]{m: m, h: h}
	v := immutable.NewSlice[listener[T]](s)
	if k.topics.CompareAndSwap(topic, old, v) {
		return
	}
	k.Subscribe(topic, m, h)
}

// Unsubscribe allows a module to unsubscribe from a topic.
func (k *Kernel[T]) Unsubscribe(topic string, m Module[T]) {
	if !k.started {
		return // Cannot unsubscribe before the kernel has started.
	}

	old, ok := k.topics.Get(topic)
	if !ok {
		return // No subscribers for this topic.
	}

	var newListeners []listener[T]
	for _, l := range old.All() {
		if l.m.Name() != m.Name() {
			newListeners = append(newListeners, l)
		}
	}

	if len(newListeners) == 0 {
		if !k.topics.CompareAndDelete(topic, old) {
			k.Unsubscribe(topic, m)
		}
		return
	}

	v := immutable.NewSlice(newListeners)
	if !k.topics.CompareAndSwap(topic, old, v) {
		k.Unsubscribe(topic, m)
	}
}

// Publish sends a message to all modules subscribed to the specified topic.
func (k *Kernel[T]) Publish(ctx context.Context, topic string, data T) error {
	if !k.started {
		return errors.New("kernel has not started, cannot publish messages")
	}
	k.topicsEqualOnce.Do(k.setupTopics)

	listeners, ok := k.topics.Get(topic)
	if !ok || listeners.Len() == 0 {
		return fmt.Errorf("no subscribers for topic %s", topic)
	}

	for _, l := range listeners.All() {
		context.Pool(ctx).Submit(
			ctx,
			func() {
				l.h(ctx, topic, data)
			},
		)
	}

	for _, l := range k.all.All() {
		context.Pool(ctx).Submit(
			ctx,
			func() {
				l.h(ctx, topic, data)
			},
		)
	}
	return nil
}

// setupTopics initializes the topics map with a custom equality function.
// This is necessary to ensure that we can compare slices of listeners correctly.
func (k *Kernel[T]) setupTopics() {
	k.topics.IsEqual = k.topicsEqual
}

// topicsEqual checks if two slices of listeners are equal. This is used to determine if a topic has changed.
func (k *Kernel[T]) topicsEqual(a, b immutable.Slice[listener[T]]) bool {
	if a.Len() != b.Len() {
		return false
	}
	if a.Len() == 0 {
		return true
	}
	if &unsafe.Slice(a)[0] == &unsafe.Slice(b)[0] {
		return true
	}
	return false
}
