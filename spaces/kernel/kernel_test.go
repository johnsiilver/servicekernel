package kernel

import (
	"errors"
	"testing"
	"time"

	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/generics/sets"
	"github.com/gostdlib/base/values/immutable"
)

// fakeModule implements Module interface for testing
type fakeModule struct {
	name        string
	initErr     error
	startErr    error
	initCalled  bool
	startCalled bool
	symbols     Symbols[string]
}

func (f *fakeModule) Name() string {
	return f.name
}

func (f *fakeModule) Init(ctx context.Context, s Symbols[string]) error {
	f.initCalled = true
	f.symbols = s
	return f.initErr
}

func (f *fakeModule) Start(ctx context.Context) error {
	f.startCalled = true
	return f.startErr
}

// moduleWrapper allows wrapping functions for Module interface
type moduleWrapper struct {
	name      string
	initFunc  func(ctx context.Context, s Symbols[string]) error
	startFunc func(ctx context.Context) error
}

func (m *moduleWrapper) Name() string {
	return m.name
}

func (m *moduleWrapper) Init(ctx context.Context, s Symbols[string]) error {
	if m.initFunc != nil {
		return m.initFunc(ctx, s)
	}
	return nil
}

func (m *moduleWrapper) Start(ctx context.Context) error {
	if m.startFunc != nil {
		return m.startFunc(ctx)
	}
	return nil
}

func TestStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modules []*fakeModule
		started bool
		wantErr bool
	}{
		{
			name: "Success: single module starts successfully",
			modules: []*fakeModule{
				{name: "module1"},
			},
			wantErr: false,
		},
		{
			name: "Success: multiple modules start successfully",
			modules: []*fakeModule{
				{name: "module1"},
				{name: "module2"},
				{name: "module3"},
			},
			wantErr: false,
		},
		{
			name: "Error: module init fails",
			modules: []*fakeModule{
				{name: "module1"},
				{name: "module2", initErr: errors.New("init failed")},
			},
			wantErr: true,
		},
		{
			name: "Error: module start fails",
			modules: []*fakeModule{
				{name: "module1"},
				{name: "module2", startErr: errors.New("start failed")},
			},
			wantErr: true,
		},
		{
			name:    "Success: no modules registered",
			modules: []*fakeModule{},
			wantErr: false,
		},
		{
			name: "Error: already started",
			modules: []*fakeModule{
				{name: "module1"},
			},
			started: true,
			wantErr: true,
		},
	}

	for _, test := range tests {

		ctx := t.Context()
		k := &Kernel[string]{
			moduleNames: sets.Set[string]{},
			topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
			started:     test.started,
		}

		// Register modules
		for _, m := range test.modules {
			if err := k.Register(m); err != nil {
				t.Fatalf("TestKernelStart(%s): failed to register module: %v", test.name, err)
			}
		}

		err := k.Start(ctx)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestKernelStart(%s): got err == nil, want err != nil", test.name)
			return
		case err != nil && !test.wantErr:
			t.Errorf("TestKernelStart(%s): got err == %s, want err == nil", test.name, err)
			return
		case err != nil:
			return
		}

		// Verify all modules were initialized and started
		for _, m := range test.modules {
			if !m.initCalled {
				t.Errorf("TestKernelStart(%s): module %s Init() was not called", test.name, m.name)
			}
			if !m.startCalled {
				t.Errorf("TestKernelStart(%s): module %s Start() was not called", test.name, m.name)
			}
			if m.symbols == nil {
				t.Errorf("TestKernelStart(%s): module %s did not receive symbols", test.name, m.name)
			}
		}

		// Verify kernel is marked as started
		if !k.started {
			t.Errorf("TestKernelStart(%s): kernel not marked as started", test.name)
		}
	}
}

func TestRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		module          Module[string]
		existingModules []*fakeModule
		kernelStarted   bool
		wantErr         bool
	}{
		{
			name:    "Success: register valid module",
			module:  &fakeModule{name: "module1"},
			wantErr: false,
		},
		{
			name:    "Error: register nil module",
			module:  nil,
			wantErr: true,
		},
		{
			name:    "Error: register module with empty name",
			module:  &fakeModule{name: ""},
			wantErr: true,
		},
		{
			name:            "Error: register duplicate module name",
			module:          &fakeModule{name: "module1"},
			existingModules: []*fakeModule{{name: "module1"}},
			wantErr:         true,
		},
		{
			name:          "Error: register after kernel started",
			module:        &fakeModule{name: "module1"},
			kernelStarted: true,
			wantErr:       true,
		},
		{
			name:            "Success: register multiple unique modules",
			module:          &fakeModule{name: "module2"},
			existingModules: []*fakeModule{{name: "module1"}},
			wantErr:         false,
		},
	}

	for _, test := range tests {
		k := &Kernel[string]{
			moduleNames: sets.Set[string]{},
			topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
		}

		// Register existing modules
		for _, m := range test.existingModules {
			if err := k.Register(m); err != nil {
				t.Fatalf("TestKernelRegister(%s): failed to register existing module: %v", test.name, err)
			}
		}

		if test.kernelStarted {
			k.started = true
		}

		err := k.Register(test.module)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestKernelRegister(%s): got err == nil, want err != nil", test.name)
			return
		case err != nil && !test.wantErr:
			t.Errorf("TestKernelRegister(%s): got err == %s, want err == nil", test.name, err)
			return
		case err != nil:
			return
		}

		// Verify module was added
		if test.module != nil && !k.moduleNames.Contains(test.module.Name()) {
			t.Errorf("TestKernelRegister(%s): module name not in registry", test.name)
		}
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	k := &Kernel[string]{
		moduleNames: sets.Set[string]{},
		topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
	}

	modules := []string{"module1", "module2", "module3"}
	for _, name := range modules {
		if err := k.Register(&fakeModule{name: name}); err != nil {
			t.Fatalf("TestKernelRegistry: failed to register module %s: %v", name, err)
		}
	}

	registry := k.Registry()

	// Check all modules are in registry
	for _, name := range modules {
		if !registry.Contains(name) {
			t.Errorf("TestKernelRegistry: module %s not in registry", name)
		}
	}

	// Check registry size
	if registry.Len() != len(modules) {
		t.Errorf("TestKernelRegistry: registry size = %d, want %d", registry.Len(), len(modules))
	}

	// Verify returned registry is a copy (not original)
	registry.Add("newModule")
	if k.moduleNames.Contains("newModule") {
		t.Errorf("TestKernelRegistry: modifying returned registry affected kernel's internal registry")
	}
}

func TestSubscribe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k := &Kernel[string]{}

	module1 := &fakeModule{name: "module1"}
	module2 := &fakeModule{name: "module2"}
	module3 := &fakeModule{name: "module3"}

	// Register and start kernel
	if err := k.Register(module1); err != nil {
		t.Fatalf("TestKernelSubscribe: failed to register module1: %v", err)
	}
	if err := k.Register(module2); err != nil {
		t.Fatalf("TestKernelSubscribe: failed to register module2: %v", err)
	}
	if err := k.Register(module3); err != nil {
		t.Fatalf("TestKernelSubscribe: failed to register module3: %v", err)
	}
	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestKernelSubscribe: failed to start kernel: %v", err)
	}

	handler := func(ctx context.Context, topic string, data string) {
		// Handler implementation for testing
	}

	// Subscribe module1 to topic
	k.Subscribe("topic1", module1, handler)

	// Check subscription exists
	listeners, ok := k.topics.Get("topic1")
	if !ok {
		t.Errorf("TestKernelSubscribe: topic1 not found in topics map")
	}
	if listeners.Len() != 1 {
		t.Errorf("TestKernelSubscribe: expected 1 listener, got %d", listeners.Len())
	}

	// Subscribe same module again (should not duplicate)
	k.Subscribe("topic1", module1, handler)
	listeners, _ = k.topics.Get("topic1")
	if listeners.Len() != 1 {
		t.Errorf("TestKernelSubscribe: duplicate subscription created, expected 1 listener, got %d", listeners.Len())
	}

	// Subscribe different module to same topic
	k.Subscribe("topic1", module2, handler)
	listeners, _ = k.topics.Get("topic1")
	if listeners.Len() != 2 {
		t.Errorf("TestKernelSubscribe: expected 2 listeners after second module subscription, got %d", listeners.Len())
	}

	// Test wildcard subscription
	k.Subscribe("*", module3, handler)
	
	// Check wildcard subscription is in all map
	if _, ok := k.all.Get("module3"); !ok {
		t.Errorf("TestKernelSubscribe: module3 not found in all map after wildcard subscription")
	}
	
	// Wildcard subscription should NOT appear in regular topics map
	if _, ok := k.topics.Get("*"); ok {
		t.Errorf("TestKernelSubscribe: wildcard (*) should not be in topics map")
	}
	
	// Subscribe same module to wildcard again (should overwrite, not error)
	k.Subscribe("*", module3, handler)
	if _, ok := k.all.Get("module3"); !ok {
		t.Errorf("TestKernelSubscribe: module3 wildcard subscription was removed on re-subscribe")
	}
}

func TestUnsubscribe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k := &Kernel[string]{
		moduleNames: sets.Set[string]{},
		topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
	}

	module1 := &fakeModule{name: "module1"}
	module2 := &fakeModule{name: "module2"}
	module3 := &fakeModule{name: "module3"}

	if err := k.Register(module1); err != nil {
		t.Fatalf("TestKernelUnsubscribe: failed to register module1: %v", err)
	}
	if err := k.Register(module2); err != nil {
		t.Fatalf("TestKernelUnsubscribe: failed to register module2: %v", err)
	}
	if err := k.Register(module3); err != nil {
		t.Fatalf("TestKernelUnsubscribe: failed to register module3: %v", err)
	}
	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestKernelUnsubscribe: failed to start kernel: %v", err)
	}

	handler := func(ctx context.Context, topic string, data string) {}

	// Subscribe modules to topic
	k.Subscribe("topic1", module1, handler)
	k.Subscribe("topic1", module2, handler)
	k.Subscribe("topic1", module3, handler)

	// Unsubscribe module2
	k.Unsubscribe("topic1", module2)

	listeners, ok := k.topics.Get("topic1")
	if !ok {
		t.Errorf("TestKernelUnsubscribe: topic1 was removed when it still has subscribers")
	}
	if listeners.Len() != 2 {
		t.Errorf("TestKernelUnsubscribe: expected 2 listeners after unsubscribe, got %d", listeners.Len())
	}

	// Verify module2 is not in listeners
	for _, l := range listeners.All() {
		if l.m.Name() == module2.Name() {
			t.Errorf("TestKernelUnsubscribe: module2 still in listeners after unsubscribe")
		}
	}

	// Unsubscribe remaining modules
	k.Unsubscribe("topic1", module1)
	k.Unsubscribe("topic1", module3)

	// Topic should be removed when no subscribers
	_, ok = k.topics.Get("topic1")
	if ok {
		t.Errorf("TestKernelUnsubscribe: topic1 still exists with no subscribers")
	}

	// Unsubscribe from non-existent topic (should not panic)
	k.Unsubscribe("nonexistent", module1)

	// Unsubscribe non-subscribed module (should not panic)
	k.Subscribe("topic2", module1, handler)
	k.Unsubscribe("topic2", module2)
}

func TestPublish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		topic             string
		subscribers       int
		wildcardSubscribers int
		kernelStarted     bool
		wantErr           bool
	}{
		{
			name:          "Success: publish to topic with subscribers",
			topic:         "topic1",
			subscribers:   2,
			kernelStarted: true,
			wantErr:       false,
		},
		{
			name:          "Error: publish to topic with no subscribers",
			topic:         "topic1",
			subscribers:   0,
			kernelStarted: true,
			wantErr:       true,
		},
		{
			name:          "Error: publish before kernel started",
			topic:         "topic1",
			subscribers:   1,
			kernelStarted: false,
			wantErr:       true,
		},
		{
			name:                "Success: publish with wildcard subscribers",
			topic:               "topic1",
			subscribers:         1,
			wildcardSubscribers: 2,
			kernelStarted:       true,
			wantErr:             false,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		k := &Kernel[string]{
			moduleNames: sets.Set[string]{},
			topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
			all:         sync.ShardedMap[string, listener[string]]{},
		}

		var mu sync.Mutex
		receivedCount := 0
		handler := func(ctx context.Context, topic string, data string) {
			mu.Lock()
			defer mu.Unlock()
			receivedCount++
		}

		// Register modules for specific topic subscribers
		modules := make([]*fakeModule, test.subscribers)
		for i := 0; i < test.subscribers; i++ {
			modules[i] = &fakeModule{name: string(rune('a' + i))}
			if err := k.Register(modules[i]); err != nil {
				t.Fatalf("TestKernelPublish(%s): failed to register module: %v", test.name, err)
			}
		}
		
		// Register modules for wildcard subscribers
		wildcardModules := make([]*fakeModule, test.wildcardSubscribers)
		for i := 0; i < test.wildcardSubscribers; i++ {
			wildcardModules[i] = &fakeModule{name: "wildcard" + string(rune('a' + i))}
			if err := k.Register(wildcardModules[i]); err != nil {
				t.Fatalf("TestKernelPublish(%s): failed to register wildcard module: %v", test.name, err)
			}
		}

		// Start kernel if needed
		if test.kernelStarted {
			if err := k.Start(ctx); err != nil {
				t.Fatalf("TestKernelPublish(%s): failed to start kernel: %v", test.name, err)
			}
			// Subscribe after starting
			for i := 0; i < test.subscribers; i++ {
				k.Subscribe(test.topic, modules[i], handler)
			}
			// Subscribe wildcard listeners
			for i := 0; i < test.wildcardSubscribers; i++ {
				k.Subscribe("*", wildcardModules[i], handler)
			}
		}

		err := k.Publish(ctx, test.topic, "test data")

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestKernelPublish(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestKernelPublish(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		// Give handlers time to execute (they run in goroutines)
		time.Sleep(50 * time.Millisecond)

		// Verify all subscribers received the message
		mu.Lock()
		expectedCount := test.subscribers + test.wildcardSubscribers
		if receivedCount != expectedCount {
			t.Errorf("TestKernelPublish(%s): received %d messages, expected %d (subscribers: %d, wildcard: %d)", 
				test.name, receivedCount, expectedCount, test.subscribers, test.wildcardSubscribers)
		}
		mu.Unlock()
	}
}

func TestPublishConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k := &Kernel[string]{
		moduleNames: sets.Set[string]{},
		topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
	}

	module := &fakeModule{name: "module1"}
	if err := k.Register(module); err != nil {
		t.Fatalf("TestKernelPublishConcurrent: failed to register module: %v", err)
	}
	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestKernelPublishConcurrent: failed to start kernel: %v", err)
	}

	var mu sync.Mutex
	var receivedData []string
	handler := func(ctx context.Context, topic string, data string) {
		mu.Lock()
		defer mu.Unlock()
		receivedData = append(receivedData, data)
	}

	k.Subscribe("topic1", module, handler)

	// Publish multiple messages concurrently
	messageCount := 100
	for i := 0; i < messageCount; i++ {
		go func(n int) {
			data := string(rune('a' + (n % 26)))
			k.Publish(ctx, "topic1", data)
		}(i)
	}

	// Wait for handlers to complete
	time.Sleep(100 * time.Millisecond)

	// Verify all messages were received (order not guaranteed)
	mu.Lock()
	receivedCount := len(receivedData)
	mu.Unlock()
	if receivedCount != messageCount {
		t.Errorf("TestKernelPublishConcurrent: received %d messages, expected %d", receivedCount, messageCount)
	}
}

func TestSubscribeConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k := &Kernel[string]{
		moduleNames: sets.Set[string]{},
		topics:      sync.ShardedMap[string, immutable.Slice[listener[string]]]{},
	}

	// Register many modules
	moduleCount := 50
	modules := make([]*fakeModule, moduleCount)
	for i := 0; i < moduleCount; i++ {
		modules[i] = &fakeModule{name: string(rune('a'+(i%26))) + string(rune('0'+(i/26)))}
		if err := k.Register(modules[i]); err != nil {
			t.Fatalf("TestKernelSubscribeConcurrent: failed to register module %d: %v", i, err)
		}
	}

	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestKernelSubscribeConcurrent: failed to start kernel: %v", err)
	}

	handler := func(ctx context.Context, topic string, data string) {}

	// Subscribe concurrently
	for i, module := range modules {
		go func(m Module[string], topic string) {
			k.Subscribe(topic, m, handler)
		}(module, "topic"+string(rune('0'+(i%5))))
	}

	// Wait for subscriptions to complete
	time.Sleep(100 * time.Millisecond)

	// Verify subscriptions
	for i := 0; i < 5; i++ {
		topic := "topic" + string(rune('0'+i))
		listeners, ok := k.topics.Get(topic)
		if !ok {
			t.Errorf("TestKernelSubscribeConcurrent: topic %s not found", topic)
			continue
		}
		// Each topic should have approximately moduleCount/5 listeners
		expectedCount := moduleCount / 5
		if listeners.Len() != expectedCount {
			t.Errorf("TestKernelSubscribeConcurrent: topic %s has %d listeners, expected ~%d", topic, listeners.Len(), expectedCount)
		}
	}
}
