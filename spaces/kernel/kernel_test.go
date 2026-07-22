package kernel

import (
	"strings"
	"testing"
	"time"

	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/concurrency/broadcast/subscriber"
)

// fakeModule implements Module interface for testing.
type fakeModule struct {
	name        string
	initErr     error
	startErr    error
	initCalled  bool
	startCalled bool
	api         API[string]
}

func (f *fakeModule) Name() string {
	return f.name
}

func (f *fakeModule) Init(ctx context.Context, s API[string]) error {
	f.initCalled = true
	f.api = s
	return f.initErr
}

func (f *fakeModule) Start(ctx context.Context) error {
	f.startCalled = true
	return f.startErr
}

// failingModule fails in whichever hook it was given an error for, after running probe. It exists so the
// teardown that follows a failed Start can be exercised from the Init path and the Start path alike.
type failingModule struct {
	name     string
	initErr  error
	startErr error
	probe    func()
}

func (f *failingModule) Name() string { return f.name }

func (f *failingModule) Init(ctx context.Context, api API[string]) error {
	if f.probe != nil {
		f.probe()
	}
	return f.initErr
}

func (f *failingModule) Start(ctx context.Context) error { return f.startErr }

// subscribingModule subscribes to a topic during Init. It exists to exercise what happens to subscriptions
// registered by an earlier module when a later module fails to start.
type subscribingModule struct {
	name  string
	topic string
	h     Handler[string]
}

func (s *subscribingModule) Name() string { return s.name }

func (s *subscribingModule) Init(ctx context.Context, api API[string]) error {
	if s.h != nil {
		return api.Subscribe(s.topic, s, s.h)
	}
	return nil
}

func (s *subscribingModule) Start(ctx context.Context) error { return nil }

// nopHandler is a Handler for tests that only care that a call was accepted, not what it delivered.
func nopHandler(ctx context.Context, topic, data string) error { return nil }

// waitFor blocks until cond reports true, failing the test if it has not within five seconds. It exists for
// the teardowns that land asynchronously through the lifetime context's watch, which a fixed sleep would
// only appear to synchronize with.
func waitFor(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("%s: timed out waiting for %s", tb.Name(), what)
		}
		time.Sleep(time.Millisecond)
	}
}

// wantRefusesWork asserts that a dead kernel turns away everything: it cannot be published to, and it can
// neither be registered into nor restarted. The ways a kernel dies (Stop, a failed Start) all have to leave
// it in this state, so each of their tests ends here.
//
// Publish reports the refusal; Register and Start panic, because using a kernel that is already finished is
// a programmer error rather than a runtime condition.
func wantRefusesWork(tb testing.TB, name string, k *Kernel[string], ctx context.Context) {
	tb.Helper()

	if err := k.Publish(ctx, "topic1", "x"); err == nil {
		tb.Errorf("%s(%s): Publish on a dead kernel: got err == nil, want err != nil", tb.Name(), name)
	}
	wantPanic(tb, tb.Name(), name+": Register on a dead kernel", true, func() { k.Register(&fakeModule{name: "afterwards"}) })
	wantPanic(tb, tb.Name(), name+": Start on a dead kernel", true, func() { k.Start(ctx) })
}

// wantPanic runs f and asserts whether it panicked. Every panic contract in the package is asserted through
// this, so the two directions are always checked the same way and a table only has to say which it expects.
// test names the row, what names the call being made.
func wantPanic(tb testing.TB, test, what string, want bool, f func()) {
	tb.Helper()

	defer func() {
		switch r := recover(); {
		case r == nil && want:
			tb.Errorf("%s(%s): %s: got no panic, want a panic", test, what, tb.Name())
		case r != nil && !want:
			tb.Errorf("%s(%s): %s: got panic %v, want none", test, what, tb.Name(), r)
		}
	}()
	f()
}

// delivery is a single message a collector received.
type delivery struct {
	topic string
	data  string
}

// collector is a test Handler that records every delivery and lets a test block until an exact
// number of deliveries have landed, replacing time.Sleep based waits with deterministic ones.
type collector struct {
	mu  sync.Mutex
	got []delivery
	ch  chan delivery
	// overflowed records that a delivery could not be buffered, which means the test published more than
	// the collector can hold without draining. It is a test bug rather than a kernel one, so it is surfaced
	// rather than allowed to look like a missing delivery.
	overflowed sync.AtomicValue[bool]
}

func newCollector() *collector {
	// Buffered well above any single test's undrained burst but far below what would let a runaway
	// delivery loop go unnoticed: the overflow guard is only a real check if the buffer can be filled.
	return &collector{ch: make(chan delivery, 512)}
}

// handler is the Handler to hand to Subscribe.
func (c *collector) handler(ctx context.Context, topic, data string) error {
	c.mu.Lock()
	c.got = append(c.got, delivery{topic: topic, data: data})
	c.mu.Unlock()

	// Non-blocking: a blocking send past the buffer would wedge the delivery loop and hold a background
	// worker for the rest of the run, so a test that over-publishes would hang rather than fail. Overflow
	// is recorded instead, and wait reports it.
	select {
	case c.ch <- delivery{topic: topic, data: data}:
	default:
		c.overflowed.Store(true)
	}
	return nil
}

// wait blocks until n deliveries have landed or the test times out.
func (c *collector) wait(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c.ch:
		case <-time.After(5 * time.Second):
			if c.overflowed.Load() {
				t.Fatalf("%s: collector buffer overflowed; the test published more than it drained", t.Name())
			}
			t.Fatalf("%s: timed out waiting for delivery %d of %d", t.Name(), i+1, n)
		}
	}
	// Also checked on the success path: an overflow that still left n items buffered would otherwise pass
	// while deliveries had been silently dropped.
	if c.overflowed.Load() {
		t.Fatalf("%s: collector buffer overflowed; the test published more than it drained", t.Name())
	}
}

// expectNoMore asserts no further delivery arrives within a short window.
func (c *collector) expectNoMore(t *testing.T) {
	t.Helper()
	select {
	case d := <-c.ch:
		t.Fatalf("%s: unexpected extra delivery: %+v", t.Name(), d)
	case <-time.After(200 * time.Millisecond):
	}
}

// received returns a copy of every delivery recorded so far.
func (c *collector) received() []delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]delivery, len(c.got))
	copy(out, c.got)
	return out
}

// doSubscribe subscribes and fails on error. It takes a testing.TB so benchmarks share it with tests.
func doSubscribe(tb testing.TB, k *Kernel[string], topic string, m Module[string], h Handler[string]) {
	tb.Helper()
	if err := k.Subscribe(topic, m, h); err != nil {
		tb.Fatalf("%s: subscribe %s: %v", tb.Name(), topic, err)
	}
}

// driveTo moves an already-registered kernel to state s through its real transitions. It never fabricates
// the state with a store, which would build a kernel claiming to run with a nil baseCtx and an unopened bus.
func driveTo(tb testing.TB, ctx context.Context, k *Kernel[string], s state) {
	tb.Helper()
	switch s {
	case unknownState: // Nothing to do: a registered kernel is already here.
	case runningState:
		if err := k.Start(ctx); err != nil {
			tb.Fatalf("%s: priming Start: %v", tb.Name(), err)
		}
	case stoppedState:
		k.Stop(ctx)
	default:
		tb.Fatalf("%s: driveTo: unhandled state %v", tb.Name(), s)
	}
}

// kernelInState returns a kernel holding one registered module and driven to s through its real transitions,
// for the tables whose rows vary only by lifecycle state. It never fabricates the state with a store, which
// would build a kernel claiming to run with a nil baseCtx and an unopened bus.
func kernelInState(tb testing.TB, ctx context.Context, s state) (*Kernel[string], *fakeModule) {
	tb.Helper()

	k := &Kernel[string]{}
	m := &fakeModule{name: "m"}
	k.Register(m)
	driveTo(tb, ctx, k, s)
	return k, m
}

// startKernel registers a fakeModule per name and starts the kernel. It takes a testing.TB so benchmarks
// share it with tests.
func startKernel(tb testing.TB, ctx context.Context, names ...string) (*Kernel[string], map[string]*fakeModule) {
	tb.Helper()

	k := &Kernel[string]{}
	mods := map[string]*fakeModule{}
	for _, n := range names {
		m := &fakeModule{name: n}
		k.Register(m)
		mods[n] = m
	}
	if err := k.Start(ctx); err != nil {
		tb.Fatalf("%s: start: %v", tb.Name(), err)
	}
	return k, mods
}

func TestStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modules []*fakeModule
		state   state
		// kName is the Kernel.Name to start with. Nothing else sets it, so without these rows Start's whole
		// name gate could be deleted unnoticed.
		kName     string
		cancelCtx bool
		// wantPanic covers calling Start on a kernel that has already been started or stopped, which is a
		// programmer error the kernel panics on rather than reporting.
		wantPanic bool
		wantErr   bool
	}{
		{
			name:    "Success: single module starts successfully",
			modules: []*fakeModule{{name: "module1"}},
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
			name:      "Error: a second Start panics",
			modules:   []*fakeModule{{name: "module1"}},
			state:     runningState,
			wantPanic: true,
		},
		{
			name:      "Error: Start on a stopped kernel panics",
			modules:   []*fakeModule{{name: "module1"}},
			state:     stoppedState,
			wantPanic: true,
		},
		{
			name:    "Success: a valid name is accepted",
			modules: []*fakeModule{{name: "module1"}},
			kName:   "svc/kernel",
			wantErr: false,
		},
		{
			name:    "Error: an invalid name fails Start",
			modules: []*fakeModule{{name: "module1"}},
			kName:   "*",
			wantErr: true,
		},
		{
			name:      "Error: start with an already cancelled context",
			modules:   []*fakeModule{{name: "module1"}},
			cancelCtx: true,
			wantErr:   true,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		if test.cancelCtx {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}

		k := &Kernel[string]{Name: test.kName}

		for _, m := range test.modules {
			k.Register(m)
		}

		driveTo(t, ctx, k, test.state)

		var err error
		wantPanic(t, "TestStart", test.name, test.wantPanic, func() { err = k.Start(ctx) })
		if test.wantPanic {
			continue
		}

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestStart(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestStart(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		// The name the bus records metrics under is the resolved one, so this also pins meterName's default.
		wantBus := test.kName
		if wantBus == "" {
			wantBus = defaultName
		}
		if k.bus.Name != wantBus {
			t.Errorf("TestStart(%s): got bus name %q, want %q", test.name, k.bus.Name, wantBus)
		}

		for _, m := range test.modules {
			if !m.initCalled {
				t.Errorf("TestStart(%s): module %s Init() was not called", test.name, m.name)
			}
			if !m.startCalled {
				t.Errorf("TestStart(%s): module %s Start() was not called", test.name, m.name)
			}
			if m.api == nil {
				t.Errorf("TestStart(%s): module %s did not receive api", test.name, m.name)
			}
		}

		if got := k.state.Load(); got != runningState {
			t.Errorf("TestStart(%s): got state == %v, want state == %v", test.name, got, runningState)
		}
	}
}

// TestStartFailureCleanup pins that a kernel which fails to start does not leave the subscriptions an
// earlier module registered live, and does not go on accepting messages.
func TestStartFailureCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// initErr and startErr pick which hook the second module fails in. Teardown must run either way;
		// only the Init path was covered before, so a Start-path teardown could be deleted unnoticed.
		initErr  error
		startErr error
	}{
		{name: "Error: a module failing Init tears the kernel down", initErr: errors.New("init failed")},
		{name: "Error: a module failing Start tears the kernel down", startErr: errors.New("start failed")},
	}

	for _, test := range tests {
		testStartFailureCleanup(t, test.name, test.initErr, test.startErr)
	}
}

// testStartFailureCleanup runs one row of TestStartFailureCleanup. It is a function rather than an inline
// loop body because it holds the assertions, and keeping them here keeps each failure reported on its own
// line rather than all of them on the loop.
func testStartFailureCleanup(t *testing.T, name string, initErr, startErr error) {
	ctx := t.Context()
	c := newCollector()

	k := &Kernel[string]{}

	// Recorded from bad's Init, the one point where good's subscription is guaranteed live. Without this
	// baseline every assertion below would hold just as well if good had never subscribed at all, so a
	// broken Subscribe would leave the test passing.
	liveAtFailure := -1
	good := &subscribingModule{name: "good", topic: "topic1", h: c.handler}
	bad := &failingModule{
		name:     "bad",
		initErr:  initErr,
		startErr: startErr,
		probe: func() {
			k.subsMu.Lock()
			liveAtFailure = len(k.subs)
			k.subsMu.Unlock()
		},
	}

	k.Register(good)
	k.Register(bad)

	err := k.Start(ctx)
	if err == nil {
		t.Fatalf("testStartFailureCleanup(%s): got err == nil from Start, want err != nil", name)
	}

	if !errors.Is(err, ErrModuleFailed) {
		t.Errorf("testStartFailureCleanup(%s): got err == %v, want it to match ErrModuleFailed", name, err)
	}
	if !errors.Is(err, errors.ErrPermanent) {
		t.Errorf("testStartFailureCleanup(%s): got a module failure that is not permanent: %v", name, err)
	}
	if liveAtFailure != 1 {
		t.Fatalf("testStartFailureCleanup(%s): got %d live subscription topics at the moment of failure, want 1", name, liveAtFailure)
	}

	// A kernel that failed to start must not accept publishes.
	if err := k.Publish(ctx, "topic1", "x"); err == nil {
		t.Errorf("TestStartFailureCleanup: Publish after failed Start: got err == nil, want err != nil")
	}
	k.subsMu.Lock()
	live := len(k.subs)
	k.subsMu.Unlock()
	if live != 0 {
		t.Errorf("testStartFailureCleanup(%s): got %d live subscription topics after failed Start, want 0", name, live)
	}

	// Start failure closed the bus, which can never reopen, so the kernel is dead rather than merely
	// un-started. Letting it be registered into and restarted would produce a Start that reports success
	// and a kernel whose every Publish then fails.
	wantRefusesWork(t, name, k, ctx)
}

// TestStartContextCancel pins that cancelling the context passed to Start does not stop the kernel. That
// context supplies values and a span; ending the kernel is Stop's job alone. The kernel's own lifetime
// context is therefore built without ctx's cancellation, so a cancelled ctx must leave every subscription
// delivering rather than killing the delivery loops while the bus stays open and the state stays running,
// which would drop publishes in silence.
func TestStartContextCancel(t *testing.T) {
	t.Parallel()

	base := t.Context()
	ctx, cancel := context.WithCancel(base)

	k := &Kernel[string]{}
	m := &fakeModule{name: "m"}
	k.Register(m)
	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestStartContextCancel: start: %v", err)
	}

	c := newCollector()
	doSubscribe(t, k, "topic1", m, c.handler)
	if err := k.Publish(ctx, "topic1", "a"); err != nil {
		t.Fatalf("TestStartContextCancel: publish before cancel: %v", err)
	}
	c.wait(t, 1)

	cancel()

	// The kernel must be untouched: still running, its lifetime context still live, and still delivering.
	if got := k.state.Load(); got != runningState {
		t.Fatalf("TestStartContextCancel: got state == %v after cancelling the Start context, want %v", got, runningState)
	}
	if k.baseCtx.Err() != nil {
		t.Error("TestStartContextCancel: the kernel lifetime context died with the Start context")
	}
	// Published on the uncancelled parent: a delivery here proves the subscription's loop is still running,
	// which is what a lifetime context derived from ctx would have ended.
	if err := k.Publish(base, "topic1", "b"); err != nil {
		t.Fatalf("TestStartContextCancel: publish after cancel: %v", err)
	}
	c.wait(t, 1)

	// And Stop still works on it afterwards, so the kernel is merely alive rather than wedged.
	k.Stop(base)
	wantRefusesWork(t, "after Stop", k, base)
}

func TestRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		module          Module[string]
		existingModules []*fakeModule
		state           state
		// wantPanic is set for everything Register refuses. It returns nothing, so refusing is all it can
		// do: a bad module or a kernel past registration is a programmer error, not a runtime condition.
		wantPanic bool
	}{
		{
			name:   "Success: register valid module",
			module: &fakeModule{name: "module1"},
		},
		{
			name:            "Success: register multiple unique modules",
			module:          &fakeModule{name: "module2"},
			existingModules: []*fakeModule{{name: "module1"}},
		},
		{
			name:      "Error: registering a nil module panics",
			module:    nil,
			wantPanic: true,
		},
		{
			name:      "Error: registering a module with an empty name panics",
			module:    &fakeModule{name: ""},
			wantPanic: true,
		},
		{
			name:            "Error: registering a duplicate module name panics",
			module:          &fakeModule{name: "module1"},
			existingModules: []*fakeModule{{name: "module1"}},
			wantPanic:       true,
		},
		{
			name:      "Error: registering after Start panics",
			module:    &fakeModule{name: "module1"},
			state:     runningState,
			wantPanic: true,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		k := &Kernel[string]{}

		for _, m := range test.existingModules {
			k.Register(m)
		}

		driveTo(t, ctx, k, test.state)

		wantPanic(t, "TestRegister", test.name, test.wantPanic, func() { k.Register(test.module) })
		if test.wantPanic {
			continue
		}

		found := false
		for _, m := range k.modules {
			if m.Name() == test.module.Name() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("TestRegister(%s): registered module is not in the modules slice", test.name)
		}
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k := &Kernel[string]{}

	modules := []string{"module1", "module2", "module3"}
	for _, name := range modules {
		k.Register(&fakeModule{name: name})
	}

	// The registry does not exist until Start freezes it, and asking for it before then is a programmer
	// error the kernel panics on rather than answering with a half-built set.
	wantPanic(t, "TestRegistry", "Registry before Start", true, func() { k.Registry() })

	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestRegistry: start: %v", err)
	}

	registry := k.Registry()
	for _, name := range modules {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("TestRegistry: module %s not in registry", name)
		}
	}
	if registry.Len() != len(modules) {
		t.Errorf("TestRegistry: registry size = %d, want %d", registry.Len(), len(modules))
	}

	// Registering after Start is refused, which is what makes the frozen registry safe to hand out without
	// copying: there is no way for the wrapped map to change under a caller holding it.
	wantPanic(t, "TestRegistry", "Register after Start", true, func() { k.Register(&fakeModule{name: "module4"}) })
	if got := registry.Len(); got != len(modules) {
		t.Errorf("TestRegistry: registry grew to %d after a refused Register, want %d", got, len(modules))
	}
}

// kernelErrs is every error the kernel returns, in a fixed order. TestErrorClassification names the one a
// row must match by index, and asserts none of the others do, so a row cannot pass by accident and a
// sentinel that was defined wrong is caught rather than silently matching whatever it was compared against.
var kernelErrs = []error{
	ErrStoppedOrNotStarted,
	ErrInvalidArg,
	ErrModuleFailed,
	ErrBus,
}

// Indices into kernelErrs, named so the table rows read as something other than magic numbers.
const (
	errStoppedOrNotStarted = iota
	errInvalidArg
	errModuleFailed
	errBus
)

// TestErrorClassification pins which error each condition returns and whether it is permanent. Callers
// branch on these with errors.Is, and a caller running a kernel call under retry/exponential depends on the
// permanent marking to stop rather than retry something that can never succeed.
func TestErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		call  func(ctx context.Context, k *Kernel[string]) error
		state state
		// wantErr false means the call must succeed, in which case the fields below are unused. Without such
		// a row this table would pass against a kernel whose every call errored.
		wantErr bool
		// wantErrIs indexes kernelErrs: exactly that one must match, and every other must not.
		wantErrIs     int
		wantPermanent bool
	}{
		{
			name:    "Success: publish on a running kernel returns no error",
			call:    func(ctx context.Context, k *Kernel[string]) error { return k.Publish(ctx, "topic1", "x") },
			state:   runningState,
			wantErr: false,
		},
		{
			name:          "Error: publish before start is not running and permanent",
			call:          func(ctx context.Context, k *Kernel[string]) error { return k.Publish(ctx, "topic1", "x") },
			state:         unknownState,
			wantErr:       true,
			wantErrIs:     errStoppedOrNotStarted,
			wantPermanent: true,
		},
		{
			name:          "Error: publish after stop is not running and permanent",
			call:          func(ctx context.Context, k *Kernel[string]) error { return k.Publish(ctx, "topic1", "x") },
			state:         stoppedState,
			wantErr:       true,
			wantErrIs:     errStoppedOrNotStarted,
			wantPermanent: true,
		},
		{
			name: "Error: subscribe before start is not running and permanent",
			call: func(ctx context.Context, k *Kernel[string]) error {
				return k.Subscribe("topic1", &fakeModule{name: "x"}, nopHandler)
			},
			state:         unknownState,
			wantErr:       true,
			wantErrIs:     errStoppedOrNotStarted,
			wantPermanent: true,
		},
		{
			name:          "Error: subscribing with a nil module is a bad argument and permanent",
			call:          func(ctx context.Context, k *Kernel[string]) error { return k.Subscribe("topic1", nil, nopHandler) },
			state:         runningState,
			wantErr:       true,
			wantErrIs:     errInvalidArg,
			wantPermanent: true,
		},
		{
			name: "Error: subscribing with an empty module name is a bad argument and permanent",
			call: func(ctx context.Context, k *Kernel[string]) error {
				return k.Subscribe("topic1", &fakeModule{}, nopHandler)
			},
			state:         runningState,
			wantErr:       true,
			wantErrIs:     errInvalidArg,
			wantPermanent: true,
		},
		{
			name: "Error: subscribing with no handler is a bad argument and permanent",
			call: func(ctx context.Context, k *Kernel[string]) error {
				return k.Subscribe("topic1", &fakeModule{name: "x"}, nil)
			},
			state:         runningState,
			wantErr:       true,
			wantErrIs:     errInvalidArg,
			wantPermanent: true,
		},
	}

	for _, test := range tests {
		ctx := t.Context()

		k, m := kernelInState(t, ctx, test.state)

		err := test.call(ctx, k)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestErrorClassification(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestErrorClassification(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err == nil:
			// Nothing to match, but the row must prove the call did its job rather than only that it
			// returned nil: a Publish that silently dropped everything would otherwise satisfy it.
			c := newCollector()
			doSubscribe(t, k, "topic1", m, c.handler)
			if perr := k.Publish(ctx, "topic1", "x"); perr != nil {
				t.Errorf("TestErrorClassification(%s): publish on the success row: %v", test.name, perr)
				continue
			}
			c.wait(t, 1)
			continue
		}

		// Exactly one sentinel matches. Asserting the others do not is what catches a sentinel defined as a
		// duplicate of another, which a single positive check could never see.
		for i, want := range kernelErrs {
			wantMatch := i == test.wantErrIs
			if got := errors.Is(err, want); got != wantMatch {
				t.Errorf("TestErrorClassification(%s): errors.Is(err, kernelErrs[%d]) == %v, want %v", test.name, i, got, wantMatch)
			}
		}
		if got := errors.Is(err, errors.ErrPermanent); got != test.wantPermanent {
			t.Errorf("TestErrorClassification(%s): got Is(err, ErrPermanent) == %v, want %v", test.name, got, test.wantPermanent)
		}
	}
}

// TestRegistryAfterStop pins that stopping a kernel does not empty its registry. The registry is frozen at
// Start and handed out without copying, so a teardown must leave it intact for anyone still holding it.
func TestRegistryAfterStop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	modules := []string{"a", "b", "c"}

	k := &Kernel[string]{}
	for _, name := range modules {
		k.Register(&fakeModule{name: name})
	}
	if err := k.Start(ctx); err != nil {
		t.Fatalf("TestRegistryAfterStop: start: %v", err)
	}

	k.Stop(ctx)

	got := k.Registry()
	if got.Len() != len(modules) {
		t.Errorf("TestRegistryAfterStop: got %d modules after Stop, want %d", got.Len(), len(modules))
	}
	for _, name := range modules {
		if _, ok := got.Get(name); !ok {
			t.Errorf("TestRegistryAfterStop: module %s missing from the registry after Stop", name)
		}
	}
}

func TestSubscribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// subTopic is always subscribed. dupTopic, when set, is subscribed by the same module afterwards;
		// delivery must still happen exactly once, whether it repeats subTopic or is the other spelling of it.
		subTopic  string
		dupTopic  string
		pubTopic  string
		pubData   string
		wantTopic string
		wantData  string
	}{
		{
			name:      "Success: exact topic delivery",
			subTopic:  "topic1",
			pubTopic:  "topic1",
			pubData:   "hello",
			wantTopic: "topic1",
			wantData:  "hello",
		},
		{
			name:      "Success: duplicate subscribe delivers once",
			subTopic:  "topic1",
			dupTopic:  "topic1",
			pubTopic:  "topic1",
			pubData:   "hello",
			wantTopic: "topic1",
			wantData:  "hello",
		},
		{
			name:      "Success: subscribing to topic1 and /topic1 delivers once",
			subTopic:  "topic1",
			dupTopic:  "/topic1",
			pubTopic:  "topic1",
			pubData:   "hello",
			wantTopic: "topic1",
			wantData:  "hello",
		},
		{
			// The publish side of the same equivalence: a leading / is not part of a topic's identity, and
			// the handler sees the topic as published rather than as subscribed.
			name:      "Success: publishing with a leading slash reaches a subscriber without one",
			subTopic:  "topic1",
			pubTopic:  "/topic1",
			pubData:   "hello",
			wantTopic: "/topic1",
			wantData:  "hello",
		},
		{
			name:      "Success: wildcard delivery carries the real topic",
			subTopic:  "*",
			pubTopic:  "some/topic",
			pubData:   "x",
			wantTopic: "some/topic",
			wantData:  "x",
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		k, mods := startKernel(t, ctx, "m")

		c := newCollector()
		doSubscribe(t, k, test.subTopic, mods["m"], c.handler)
		if test.dupTopic != "" {
			doSubscribe(t, k, test.dupTopic, mods["m"], c.handler)
		}

		if err := k.Publish(ctx, test.pubTopic, test.pubData); err != nil {
			t.Fatalf("TestSubscribe(%s): publish: %v", test.name, err)
		}
		c.wait(t, 1)
		c.expectNoMore(t)

		got := c.received()
		if len(got) != 1 {
			t.Fatalf("TestSubscribe(%s): got %d deliveries, want 1: %+v", test.name, len(got), got)
		}
		if got[0].topic != test.wantTopic || got[0].data != test.wantData {
			t.Errorf("TestSubscribe(%s): got %+v, want {%s %s}", test.name, got[0], test.wantTopic, test.wantData)
		}
	}
}

// TestSubscribeTopicCanonical pins the registry bookkeeping behind the canonical topic: the two spellings
// collapse to one key, and Unsubscribe with either spelling finds it. That the two spellings deliver only
// once is covered by TestSubscribe's dupTopic row; what is asserted here is the state they leave behind.
func TestSubscribeTopicCanonical(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m")
	c := newCollector()

	doSubscribe(t, k, "topic1", mods["m"], c.handler)
	doSubscribe(t, k, "/topic1", mods["m"], c.handler)

	k.subsMu.Lock()
	keys := len(k.subs)
	k.subsMu.Unlock()
	if keys != 1 {
		t.Errorf("TestSubscribeTopicCanonical: got %d topic keys after subscribing to topic1 and /topic1, want 1", keys)
	}

	// Unsubscribing with the other spelling must remove the same subscription.
	k.Unsubscribe("/topic1", mods["m"])

	k.subsMu.Lock()
	live := len(k.subs)
	k.subsMu.Unlock()
	if live != 0 {
		t.Errorf("TestSubscribeTopicCanonical: Unsubscribe(/topic1) left %d topic keys registered, want 0", live)
	}
}

func TestUnsubscribe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m1", "m2")

	c1 := newCollector()
	c2 := newCollector()
	doSubscribe(t, k, "topic1", mods["m1"], c1.handler)
	doSubscribe(t, k, "topic1", mods["m2"], c2.handler)

	if err := k.Publish(ctx, "topic1", "a"); err != nil {
		t.Fatalf("TestUnsubscribe: publish a: %v", err)
	}
	c1.wait(t, 1)
	c2.wait(t, 1)

	k.Unsubscribe("topic1", mods["m1"])

	// publishN sends n messages and drains each through the still-subscribed m2, which is what makes the
	// send synchronous enough to reason about. The two calls below play different roles, so this is a
	// helper rather than another table: the first lets the cancellation settle, the second measures.
	publishN := func(data string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := k.Publish(ctx, "topic1", data); err != nil {
				t.Fatalf("TestUnsubscribe: publish %s: %v", data, err)
			}
			c2.wait(t, 1)
		}
	}

	// Unsubscribe is fire-and-forget, so m1 may still see a message or two already in flight.
	const batch = 50
	publishN("b", batch)

	// Once it has settled, m1 must receive nothing further.
	settled := len(c1.received())
	publishN("c", batch)
	if got := len(c1.received()); got != settled {
		t.Errorf("TestUnsubscribe: m1 received %d messages after unsubscribe settled, want %d", got, settled)
	}
}

// TestUnsubscribeGuards pins that Unsubscribe turns away a bad call quietly. It has no error return, so the
// only thing it can do is nothing: it must not panic, and it must leave the registry as it found it. The nil
// module matters most, since without the guard the call would dereference a nil interface.
func TestUnsubscribeGuards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// module is what to unsubscribe with. state is the kernel it is called on.
		module Module[string]
		state  state
	}{
		{name: "Success: a nil module is a no-op", module: nil, state: runningState},
		{name: "Success: an empty module name is a no-op", module: &fakeModule{}, state: runningState},
		{name: "Success: an unknown module is a no-op", module: &fakeModule{name: "never registered"}, state: runningState},
		{name: "Success: unsubscribing before Start is a no-op", module: &fakeModule{name: "m"}, state: unknownState},
		{name: "Success: unsubscribing after Stop is a no-op", module: &fakeModule{name: "m"}, state: stoppedState},
	}

	for _, test := range tests {
		ctx := t.Context()
		k, m := kernelInState(t, ctx, test.state)

		// A live subscription to leave undisturbed, where the kernel is running enough to take one.
		want := 0
		if test.state == runningState {
			doSubscribe(t, k, "topic1", m, nopHandler)
			want = 1
		}

		wantPanic(t, "TestUnsubscribeGuards", test.name, false, func() {
			k.Unsubscribe("topic1", test.module)
		})

		k.subsMu.Lock()
		got := len(k.subs)
		k.subsMu.Unlock()
		if got != want {
			t.Errorf("TestUnsubscribeGuards(%s): got %d live subscriptions, want %d", test.name, got, want)
		}
	}
}

func TestPublish(t *testing.T) {
	t.Parallel()

	// Every row here publishes on a started kernel and differs only in how many subscribers of each kind
	// are attached, so they all share one body. Publishing against a kernel that is not running is a
	// different shape and lives in TestErrorClassification, which also pins what those errors are.
	tests := []struct {
		name           string
		topic          string
		subscribers    int
		wildcards      int
		wantDeliveries int
	}{
		{
			name:           "Success: publish to topic with subscribers",
			topic:          "topic1",
			subscribers:    2,
			wantDeliveries: 2,
		},
		{
			// Publishing to a topic with no subscribers drops the value and returns nil rather than erroring.
			name:           "Success: publish to topic with no subscribers is dropped",
			topic:          "topic1",
			subscribers:    0,
			wantDeliveries: 0,
		},
		{
			name:           "Success: publish reaches wildcard subscribers",
			topic:          "topic1",
			subscribers:    0,
			wildcards:      2,
			wantDeliveries: 2,
		},
		{
			name:           "Success: publish reaches exact and wildcard subscribers together",
			topic:          "topic1",
			subscribers:    2,
			wildcards:      2,
			wantDeliveries: 4,
		},
	}

	for _, test := range tests {
		ctx := t.Context()

		names := make([]string, 0, test.subscribers+test.wildcards)
		for i := 0; i < test.subscribers; i++ {
			names = append(names, "s"+string(rune('a'+i)))
		}
		for i := 0; i < test.wildcards; i++ {
			names = append(names, "w"+string(rune('a'+i)))
		}
		k, mods := startKernel(t, ctx, names...)

		c := newCollector()
		for i := 0; i < test.subscribers; i++ {
			doSubscribe(t, k, test.topic, mods["s"+string(rune('a'+i))], c.handler)
		}
		for i := 0; i < test.wildcards; i++ {
			doSubscribe(t, k, "*", mods["w"+string(rune('a'+i))], c.handler)
		}

		if err := k.Publish(ctx, test.topic, "data"); err != nil {
			t.Errorf("TestPublish(%s): got err == %s, want err == nil", test.name, err)
			continue
		}

		c.wait(t, test.wantDeliveries)
		c.expectNoMore(t)
	}
}

// renderedErr records how often its Error method ran. The kernel renders a handler error when it logs it,
// and nothing else in the delivery path calls Error, so a non-zero count is evidence the error reached the
// logger rather than being dropped.
type renderedErr struct {
	rendered *sync.MutexValue[int]
}

func (r renderedErr) Error() string {
	r.rendered.WithLock(func(p *int) { *p++ })
	return "handler failed"
}

// TestHandlerErrorRouted pins what the delivery loop does with an error a handler returned: it keeps
// delivering, and it logs the error rather than dropping it.
func TestHandlerErrorRouted(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m")

	var rendered sync.MutexValue[int]
	delivered := newCollector()
	doSubscribe(t, k, "topic1", mods["m"], func(hctx context.Context, topic, data string) error {
		delivered.handler(hctx, topic, data)
		return renderedErr{rendered: &rendered}
	})

	// Three publishes, so the first handler error is also shown not to stop the deliveries after it.
	for _, d := range []string{"a", "b", "c"} {
		if err := k.Publish(ctx, "topic1", d); err != nil {
			t.Fatalf("TestHandlerErrorRouted: publish %s: %v", d, err)
		}
	}
	delivered.wait(t, 3)

	// The handler has returned by now, but the kernel classifies and logs its error afterwards, so this is
	// the one place a poll is needed rather than a delivery to wait on.
	// All three, not merely one: a delivery loop that logged the first error and then stopped logging would
	// satisfy a "more than zero" check while silently swallowing everything after it.
	waitFor(t, "every handler error to reach the logger", func() bool { return rendered.Load() == 3 })
}

func TestPublishConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m1")

	c := newCollector()
	doSubscribe(t, k, "topic1", mods["m1"], c.handler)

	messageCount := 100
	g := sync.Group{}
	for i := 0; i < messageCount; i++ {
		data := string(rune('a' + (i % 26)))
		g.Go(ctx, func(ctx context.Context) error {
			return k.Publish(ctx, "topic1", data)
		})
	}
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("TestPublishConcurrent: publish: %v", err)
	}

	c.wait(t, messageCount)
}

func TestSubscribeConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	const (
		moduleCount = 25
		topicCount  = 5
	)
	perTopic := moduleCount / topicCount

	names := make([]string, moduleCount)
	for i := 0; i < moduleCount; i++ {
		names[i] = "m" + string(rune('a'+i))
	}
	k, mods := startKernel(t, ctx, names...)

	topics := make([]string, topicCount)
	for i := 0; i < topicCount; i++ {
		topics[i] = "topic" + string(rune('0'+i))
	}

	c := newCollector()
	g := sync.Group{}
	for i := 0; i < moduleCount; i++ {
		topic := topics[i%topicCount]
		m := mods[names[i]]
		g.Go(ctx, func(ctx context.Context) error {
			return k.Subscribe(topic, m, c.handler)
		})
	}
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("TestSubscribeConcurrent: subscribe: %v", err)
	}

	// Each topic now has perTopic subscribers; one publish per topic delivers to all of them.
	for _, topic := range topics {
		if err := k.Publish(ctx, topic, "data"); err != nil {
			t.Fatalf("TestSubscribeConcurrent: publish %s: %v", topic, err)
		}
	}
	c.wait(t, perTopic*topicCount)
}

// TestSubscribeLifecycle pins which lifecycle states accept a Subscribe. The stopped cases matter beyond the
// error they return: a Subscribe that slips through on a stopped kernel registers a delivery loop that no
// teardown will ever cancel, because teardown has already snapshotted and dropped k.subs.
func TestSubscribeLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		state   state
		wantErr bool
	}{
		{
			name:    "Error: subscribe before the kernel has started",
			state:   unknownState,
			wantErr: true,
		},
		{
			name:    "Success: subscribe after the kernel has started",
			state:   runningState,
			wantErr: false,
		},
		{
			name:    "Error: subscribe after the kernel has stopped",
			state:   stoppedState,
			wantErr: true,
		},
	}

	for _, test := range tests {
		ctx := t.Context()

		// Driven through the real transitions rather than storing the state, so the stopped case exercises
		// the teardown a leaked subscription would have to survive.
		k, m := kernelInState(t, ctx, test.state)

		c := newCollector()
		err := k.Subscribe("topic1", m, c.handler)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestSubscribeLifecycle(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestSubscribeLifecycle(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			// No registry assertion here: both refusing states return from running() before any statement
			// touches k.subs, so a count check could not fail whatever the code did. The registration
			// window that can actually leak is pinned by TestStopConcurrentSubscribe.
			continue
		}

		// On success the subscription must actually deliver, not merely return no error.
		if err := k.Publish(ctx, "topic1", "x"); err != nil {
			t.Fatalf("TestSubscribeLifecycle(%s): publish: %v", test.name, err)
		}
		c.wait(t, 1)
	}
}

// TestStopConcurrentSubscribe pins the Subscribe/Stop race. Stop snapshots k.subs, drops it and only then
// cancels and closes the bus, so a Subscribe that checks the lifecycle state before Stop takes subsMu but
// registers after must not end up in the dropped map: that subscription would be cancelled by nobody and
// its delivery loop would outlive the kernel. Run with -race and -count to shake the window.
func TestStopConcurrentSubscribe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	const attempts = 50
	for attempt := 0; attempt < attempts; attempt++ {
		stopConcurrentSubscribeOnce(t, ctx, attempt)
	}
}

// stopConcurrentSubscribeOnce runs one round of the Subscribe/Stop race. It is looped because a round where
// Stop wins the lock outright exercises none of the window, and a single round could be all of those.
func stopConcurrentSubscribeOnce(t *testing.T, ctx context.Context, attempt int) {

	const subscribers = 25
	names := make([]string, 0, subscribers+1)
	names = append(names, "baseline")
	for i := 0; i < subscribers; i++ {
		names = append(names, "m"+string(rune('a'+i)))
	}
	k, mods := startKernel(t, ctx, names...)

	// Baseline: Subscribe genuinely works on this kernel before the race starts. Without it, a Subscribe
	// that always failed would satisfy every assertion below and the race would go unobserved.
	base := newCollector()
	doSubscribe(t, k, "topic1", mods["baseline"], base.handler)
	if err := k.Publish(ctx, "topic1", "a"); err != nil {
		t.Fatalf("TestStopConcurrentSubscribe: publish: %v", err)
	}
	base.wait(t, 1)

	// Each racing Subscribe either wins the lock before teardown claims the state (success) or loses it and
	// is refused. Both are legal, but every call must land in exactly one of the two buckets: a call that
	// reported success and was still dropped from the snapshot is the leak this test exists to catch.
	var countMu sync.Mutex
	succeeded, refused := 0, 0

	c := newCollector()
	g := sync.Group{}
	for i := 0; i < subscribers; i++ {
		m := mods[names[i+1]]
		g.Go(ctx, func(ctx context.Context) error {
			err := k.Subscribe("topic1", m, c.handler)
			countMu.Lock()
			defer countMu.Unlock()
			if err == nil {
				succeeded++
				return nil
			}
			refused++
			return nil
		})
	}
	g.Go(ctx, func(ctx context.Context) error {
		k.Stop(ctx)
		return nil
	})
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("TestStopConcurrentSubscribe: %v", err)
	}

	// Reported, not asserted: every goroutine increments exactly one counter and g.Wait guarantees they all
	// ran, so the sum is always subscribers and a check on it could never fail. The assertion that can fail
	// is below.
	t.Logf("TestStopConcurrentSubscribe: attempt %d: %d subscribes succeeded, %d were refused", attempt, succeeded, refused)

	// The assertion this test exists for. A Subscribe that registered into the map teardown had already
	// snapshotted would be cancelled by nobody, and its delivery loop would outlive the kernel; whether it
	// reported success or not, nothing may be left registered.
	//
	// The state is deliberately not asserted here: nothing in this test writes it except the Stop above,
	// which swaps unconditionally, so such a check could not fail. TestStartConcurrentStop races a Start
	// against the Stop and is where that invariant is actually falsifiable.
	k.subsMu.Lock()
	live := len(k.subs)
	k.subsMu.Unlock()
	if live != 0 {
		t.Errorf("TestStopConcurrentSubscribe: attempt %d: got %d topic keys registered after Stop, want 0", attempt, live)
	}
}

func TestOrdering(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m")

	c := newCollector()
	doSubscribe(t, k, "topic1", mods["m"], c.handler)

	const n = 200
	for i := 0; i < n; i++ {
		if err := k.Publish(ctx, "topic1", "d"+string(rune('0'+(i%10)))); err != nil {
			t.Fatalf("TestOrdering: publish %d: %v", i, err)
		}
	}
	c.wait(t, n)

	// A single subscriber must see values in the order they were sent.
	got := c.received()
	for i := 0; i < n; i++ {
		want := "d" + string(rune('0'+(i%10)))
		if got[i].data != want {
			t.Fatalf("TestOrdering: delivery %d = %q, want %q (out of order)", i, got[i].data, want)
		}
	}
}

func TestStop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m1", "m2")

	c := newCollector()
	doSubscribe(t, k, "topic1", mods["m1"], c.handler)
	doSubscribe(t, k, "*", mods["m2"], c.handler)

	if err := k.Publish(ctx, "topic1", "a"); err != nil {
		t.Fatalf("TestStop: publish: %v", err)
	}
	c.wait(t, 2) // topic1 subscriber + wildcard subscriber.

	// Each recorded cancel is swapped for a wrapper that counts itself and then calls the real one, so the
	// assertions below can tell a teardown that cancelled every subscription from one that only dropped the
	// map. Cancelling is what ends a delivery loop, and the loop is not otherwise observable from here.
	var cancelled sync.MutexValue[int]
	k.subsMu.Lock()
	live := 0
	for key, cancel := range k.subs {
		live++
		k.subs[key] = func() {
			cancelled.WithLock(func(p *int) { *p++ })
			cancel()
		}
	}
	k.subsMu.Unlock()
	if live == 0 {
		t.Fatal("TestStop: no live subscriptions before Stop, nothing to tear down")
	}

	k.Stop(ctx)

	// Every subscription must have been cancelled individually, not merely dropped from the map.
	if got := cancelled.Load(); got != live {
		t.Errorf("TestStop: got %d of %d subscriptions cancelled by Stop, want all of them", got, live)
	}
	// Checked straight after Stop with no wait: teardown runs inline, so by the time Stop returns the
	// lifetime context every subscription derives from is already cancelled. This is the backstop that
	// catches any subscription the map never recorded, which is a different guarantee from the count above.
	if k.baseCtx.Err() == nil {
		t.Error("TestStop: the kernel lifetime context was not cancelled by Stop")
	}
	// Asked of the bus directly rather than through Subscribe: every kernel entry point refuses on the
	// lifecycle state before it reaches the bus, so a kernel-level call would report a stopped kernel even
	// if teardown had never closed it.
	if _, err := k.bus.Subscribe(ctx, "topic3"); !errors.Is(err, subscriber.ErrClosed) {
		t.Errorf("TestStop: the message bus was not closed by Stop: got err == %v, want ErrClosed", err)
	}
	k.subsMu.Lock()
	remaining := len(k.subs)
	k.subsMu.Unlock()
	if remaining != 0 {
		t.Errorf("TestStop: got %d topic keys registered after Stop, want 0", remaining)
	}

	// After Stop the kernel turns away every call. There is no delivery assertion here: publishing is
	// refused from this point, so nothing can reach the bus for a leaked subscription to receive, and a
	// check for one could never fail.
	wantRefusesWork(t, "after Stop", k, ctx)

	// Stop is idempotent, asserted only as "does not panic". There is nothing else to check: teardown's
	// early return is an optimization, not a guard. A second run finds subs already nil, and both
	// baseCancel and bus.Close are themselves idempotent, so re-running it changes nothing observable.
	k.Stop(ctx)
}

// TestNameValid pins the metrics-namespace rules directly, including the two that a topic allows but a name
// does not, and that each rejection names the rule it broke rather than pointing at the character pattern.
func TestNameValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kName   string
		wantErr bool
		// wantReason is the sentinel the rejection must match, so the test pins which rule was broken
		// without reading the message text.
		wantReason error
	}{
		{name: "Success: an empty name means the default", kName: "", wantErr: false},
		{name: "Success: a simple name", kName: "mykernel", wantErr: false},
		{name: "Success: a path like name", kName: "svc/kernel", wantErr: false},
		// The default is substituted after validation runs, so nothing else would catch it going bad.
		{name: "Success: the default name is itself valid", kName: defaultName, wantErr: false},
		{name: "Error: a bare wildcard", kName: "*", wantErr: true, wantReason: errNameWildcard},
		{name: "Error: a trailing slash", kName: "kernel/", wantErr: true, wantReason: errNameTrailingSlash},
		{name: "Error: an empty segment", kName: "a//b", wantErr: true, wantReason: errNameEmptySegment},
		{name: "Error: a space", kName: "my kernel", wantErr: true, wantReason: errNamePattern},
	}

	for _, test := range tests {
		err := nameValid(test.kName)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestNameValid(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestNameValid(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err == nil:
			continue
		}

		if !errors.Is(err, test.wantReason) {
			t.Errorf("TestNameValid(%s): got %v, want it to match %v", test.name, err, test.wantReason)
		}
	}
}

// TestTopicPanics pins the contract the package doc states twice: an invalid topic panics rather than
// returning an error, at every entry point and whatever the kernel's state, and publishing to the bare
// wildcard panics too. TestTopicValid covers only the predicate, which is a proxy for this, not a test of it.
// TestHandlerContext pins that the handler runs on the context Publish supplied, which is the entire reason
// envelope carries a context at all: a span, deadline or request-scoped value on the publishing call has to
// reach the handler. Delivering any other context would satisfy every other test in the suite.
func TestHandlerContext(t *testing.T) {
	t.Parallel()

	type keyType struct{}

	ctx := t.Context()
	k, mods := startKernel(t, ctx, "m")

	got := make(chan any, 1)
	doSubscribe(t, k, "topic1", mods["m"], func(hctx context.Context, topic, data string) error {
		got <- hctx.Value(keyType{})
		return nil
	})

	want := "carried"
	if err := k.Publish(context.WithValue(ctx, keyType{}, want), "topic1", "x"); err != nil {
		t.Fatalf("TestHandlerContext: publish: %v", err)
	}

	select {
	case v := <-got:
		if v != want {
			t.Errorf("TestHandlerContext: handler saw context value %v, want %q", v, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TestHandlerContext: timed out waiting for delivery")
	}
}

func TestTopicPanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// call exercises one entry point with one topic. Only the topic varies between a row and its
		// success partner; the module and handler are always valid.
		call      func(ctx context.Context, k *Kernel[string], m Module[string], topic string)
		topic     string
		wantPanic bool
	}{
		{
			name: "Success: a valid topic does not panic on Subscribe",
			call: func(ctx context.Context, k *Kernel[string], m Module[string], topic string) {
				k.Subscribe(topic, m, nopHandler)
			},
			topic:     "topic1",
			wantPanic: false,
		},
		{
			name: "Error: an invalid topic panics on Subscribe",
			call: func(ctx context.Context, k *Kernel[string], m Module[string], topic string) {
				k.Subscribe(topic, m, nopHandler)
			},
			topic:     "topic[1]",
			wantPanic: true,
		},
		{
			name:      "Error: an invalid topic panics on Unsubscribe",
			call:      func(ctx context.Context, k *Kernel[string], m Module[string], topic string) { k.Unsubscribe(topic, m) },
			topic:     "topic[1]",
			wantPanic: true,
		},
		{
			name: "Error: an invalid topic panics on Publish",
			call: func(ctx context.Context, k *Kernel[string], m Module[string], topic string) {
				k.Publish(ctx, topic, "x")
			},
			topic:     "topic[1]",
			wantPanic: true,
		},
		{
			name: "Error: publishing to the bare wildcard panics",
			call: func(ctx context.Context, k *Kernel[string], m Module[string], topic string) {
				k.Publish(ctx, topic, "x")
			},
			topic:     "*",
			wantPanic: true,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		k, mods := startKernel(t, ctx, "m")

		wantPanic(t, "TestTopicPanics", test.name, test.wantPanic, func() {
			test.call(ctx, k, mods["m"], test.topic)
		})
	}
}

// TestTopicPanicsWhenNotRunning pins that topic validation does not depend on the lifecycle: an invalid
// topic panics on a kernel that has not started and on one that has stopped, rather than being masked by
// the not-running error those states would otherwise return.
func TestTopicPanicsWhenNotRunning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state state
		topic string
		// wantPanic separates the two claims: an invalid topic panics whatever the state, and a valid one
		// does not. Without the valid rows a topicValid that rejected every topic would satisfy this table.
		wantPanic bool
	}{
		{name: "Success: a valid topic does not panic before Start", state: unknownState, topic: "topic1"},
		{name: "Success: a valid topic does not panic after Stop", state: stoppedState, topic: "topic1"},
		{name: "Error: an invalid topic panics before Start", state: unknownState, topic: "topic[1]", wantPanic: true},
		{name: "Error: an invalid topic panics after Stop", state: stoppedState, topic: "topic[1]", wantPanic: true},
	}

	for _, test := range tests {
		ctx := t.Context()
		k, _ := kernelInState(t, ctx, test.state)

		var err error
		wantPanic(t, "TestTopicPanicsWhenNotRunning", test.name, test.wantPanic, func() {
			err = k.Publish(ctx, test.topic, "x")
		})
		if test.wantPanic {
			continue
		}
		// A valid topic on a kernel that is not running is refused, not panicked: the two mechanisms must
		// not be confused for one another.
		if !errors.Is(err, ErrStoppedOrNotStarted) {
			t.Errorf("TestTopicPanicsWhenNotRunning(%s): got err == %v, want it to match ErrStoppedOrNotStarted", test.name, err)
		}
	}
}

// TestTopicValidMatchesRegexp holds the hand-rolled scan in topicValid to the regexp it replaced. topicRE is
// the documented spec, quoted in the package doc, so the scan is only correct insofar as it agrees with it.
// The corpus is every string up to three bytes over an alphabet carrying one representative of each class
// the scan branches on, which reaches every combination of adjacent slashes, boundary positions and
// rejected bytes that a hand-rolled loop can get wrong.
func TestTopicValidMatchesRegexp(t *testing.T) {
	t.Parallel()

	// want is topicValid as it was written before the scan: the regexp plus the two segment rules.
	want := func(topic string) bool {
		switch {
		case topic == "*":
			return true
		case topic == "":
			return false
		case strings.HasSuffix(topic, "/"), strings.Contains(topic, "//"):
			return false
		}
		return topicRE.MatchString(topic)
	}

	alphabet := []string{"a", "1", "/", "_", ".", "-", "*", "@", "é"}
	corpus := []string{""}
	for i := 0; i < 3; i++ {
		next := []string{}
		for _, prefix := range corpus {
			for _, c := range alphabet {
				next = append(next, prefix+c)
			}
		}
		corpus = append(corpus, next...)
	}

	for _, topic := range corpus {
		if got := topicValid(topic); got != want(topic) {
			t.Errorf("TestTopicValidMatchesRegexp(%q): got %v, want %v", topic, got, want(topic))
		}
	}
}

func TestTopicValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		topic string
		want  bool
	}{
		{name: "Success: wildcard topic", topic: "*", want: true},
		{name: "Success: simple alphanumeric topic", topic: "topic1", want: true},
		{name: "Success: topic with forward slash", topic: "/path/to/topic", want: true},
		{name: "Success: topic with underscore", topic: "topic_name", want: true},
		{name: "Success: topic with dash", topic: "topic-name", want: true},
		{name: "Success: mixed valid characters", topic: "path/to/my_topic-123", want: true},
		{name: "Success: numbers only", topic: "12345", want: true},
		{name: "Error: empty topic", topic: "", want: false},
		{name: "Error: trailing slash", topic: "topic/", want: false},
		{name: "Error: empty segment", topic: "a//b", want: false},
		{name: "Error: leading empty segment", topic: "//a", want: false},
		{name: "Success: a leading slash on a single segment", topic: "/topic1", want: true},
		{name: "Success: a dot segment", topic: "path/./topic", want: true},
		{name: "Success: dots only", topic: "...", want: true},
		{name: "Error: a lone slash", topic: "/", want: false},
		{name: "Error: a non-ASCII rune", topic: "tópic", want: false},
		{name: "Error: topic with square brackets", topic: "topic[1]", want: false},
		{name: "Error: topic with curly braces", topic: "topic{name}", want: false},
		{name: "Error: topic with question mark", topic: "topic?", want: false},
		{name: "Error: topic with backslash", topic: "topic\\name", want: false},
		{name: "Error: topic with space", topic: "topic name", want: false},
		{name: "Error: topic with colon", topic: "topic:name", want: false},
		{name: "Error: topic with semicolon", topic: "topic;name", want: false},
		{name: "Error: topic with special characters", topic: "topic@#$%^&*()", want: false},
	}

	for _, test := range tests {
		got := topicValid(test.topic)
		if got != test.want {
			t.Errorf("TestTopicValid(%s): got %v, want %v", test.name, got, test.want)
		}
	}
}
