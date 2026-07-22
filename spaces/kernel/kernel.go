/*
Package kernel implements a service kernel that manages modules and their interactions via message passing.
It allows for module registration, subscription to topics, and message publishing.

Topics must be an exact match or *. A * subscriber is called for every message on the kernel, so it should be
used with care. A * is subscribe-only: publishing to it panics.

A note on topics: as of now it requires direct topic matching or a wildcard match(*). However, this will
likely be extended to support various topic matching patterns in the future. We currently test for
`^[A-Za-z0-9/_.-]+$` if the topic is not `*`, and additionally reject a trailing / or an empty segment (//).
Best practice is to use file like paths like /path/to/package/topic. Violation of the topic pattern panics.

A leading / is optional and is not part of a topic's identity, so /path/to/topic and path/to/topic are the
same topic for matching, for subscription dedup and for Unsubscribe.

Message routing is delegated to github.com/gostdlib/concurrency/broadcast/subscriber. A * subscription maps
to the subscriber library's ** pattern, which matches every topic.

A note on T: one published value is delivered to every subscriber on the topic and to every * subscriber,
without being copied. If T is a map, a slice or a pointer, all of those handlers share the same underlying
data and one handler's mutation is visible to the others and back to the publisher. The kernel cannot copy
a type parameter, so T should be a value type, or hold immutable.Map/immutable.Slice, or be a generated
Im<Type> by the immutable generators found in github.com/gostdlib/base/values/generators.
*/
package kernel

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/values/immutable"
	"github.com/gostdlib/concurrency/broadcast/subscriber"
)

// envelope carries the published topic and the publisher's context alongside the data. The subscriber.Value
// delivers only the value, so without this a wildcard subscriber would not know which topic fired and no
// subscriber would see the publisher's context (its trace span, deadline and request-scoped values).
type envelope[T any] struct {
	ctx   context.Context
	topic string
	data  T
}

//go:generate go tool github.com/gostdlib/base/values/generators/stringer -type=state -linecomment

// state is a Kernel's lifecycle. A Kernel only ever moves forward through it, unknownState -> runningState
// -> stoppedState. It reaches stoppedState via Stop or via a failed Start, and there is no way back: the
// bus a Kernel routes through cannot reopen once closed, so a stopped Kernel is dead and must be replaced.
type state uint8

const (
	// unknownState is a Kernel that has not been started.
	unknownState state = 0 // Unknown
	// runningState is a started Kernel, the only state in which it accepts Publish and Subscribe.
	runningState state = 1 // Running
	// stoppedState is a Kernel whose bus has been closed, by Stop or by a failed Start.
	stoppedState state = 2 // Stopped
)

// defaultName is the metrics namespace a Kernel uses when Name is not set.
const defaultName = "kernel"

//go:generate go tool github.com/gostdlib/base/values/generators/tuple -p subLookup key:string, name:string

// Kernel implements a service kernel. The zero value is ready to use and is the intended way to build one;
// a Kernel must not be copied after first use. A Kernel is single use: once it has been stopped, by Stop or
// by a failed Start, it cannot be restarted and a new Kernel must be created.
//
// Cancelling the context passed to Start does nothing, use Stop().
type Kernel[T any] struct {
	// Name namespaces the metrics the message bus records, and defaults to defaultName. It only needs
	// setting when a process runs more than one Kernel and their metrics must be separate. It must obey
	// the same character rules as a topic, except that a bare * is not a name; Start rejects anything else.
	// It must be set before Start.
	Name string

	mu sync.Mutex

	// registry is the set of module names that Registry hands out, built once by Start from modules.
	registry immutable.Map[string, struct{}]
	// frozen says the registry has been built.
	frozen atomic.Bool

	// modules is guarded by mu: Register appends to it and Start ranges it, both holding mu. Registry never
	// reads it, since freezeReg copies the names it needs into registry while Start still holds mu.
	modules []Module[T]

	// bus is the topic based broadcast the kernel routes all messages through.
	bus subscriber.Value[envelope[T]]

	// subsMu guards subs. subs maps a topic and module name to the cancel func that ends that subscription.
	// It backs dedup and Unsubscribe, neither of which the bus provides on its own.
	subsMu sync.Mutex
	subs   map[subLookup]context.CancelFunc

	// baseCtx is the kernel lifetime context, built at Start from the values of the context passed there
	// but not from its cancellation. Every subscription derives a cancelable child from it. It is nil until
	// Start runs. baseCancel cancels it, which teardown uses as a backstop so no delivery loop can outlive
	// the kernel even if it somehow escaped subs.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// state is an atomic so Subscribe, Unsubscribe and Publish can read it without taking mu (Subscribe
	// runs reentrantly from a module's Init/Start while Start holds mu). Both transitions out of
	// unknownState happen under subsMu: Start Stores runningState and teardown Swaps to stoppedState, so
	// neither can lose the other's write. See each for why that lock and not mu.
	state sync.AtomicValue[state]
}

// Start starts the kernel. This should be called after all modules are registered.
// A Start that fails should be considered dead.
func (k *Kernel[T]) Start(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if s := k.state.Load(); s != unknownState {
		panic("cannot call Start more than once")
	}

	if err := ctx.Err(); err != nil {
		return permanent(fmt.Errorf("kernel start: lifetime context is already done (%w): %w", err, ErrInvalidArg))
	}

	if err := nameValid(k.Name); err != nil {
		return permanent(fmt.Errorf("kernel name %q: %w: %w", k.Name, err, ErrInvalidArg))
	}

	// Frozen before any module runs, so a module calling Registry from Init or Start sees the full set.
	// Register is refused from here on, so the map this wraps can never change again.
	k.freezeReg()

	// Taken because Subscribe reads baseCtx under it and Publish reads the state without any lock, not
	// because the state could change: Start panics above unless the kernel is unstarted and holds mu for
	// its whole body, and every path to teardown needs mu.
	k.subsMu.Lock()
	// Detached from ctx's cancellation on purpose: Stop is the only thing that ends this kernel, so the
	// lifetime context must not die with the one Start was handed. It keeps ctx's values and span, which
	// is what a module subscribing from Init or Start needs, and teardown cancels it to end every
	// delivery loop at once. Deriving it from ctx directly would let a cancelled ctx kill the delivery
	// loops while the bus stayed open and the state stayed running, dropping publishes in silence.
	k.baseCtx, k.baseCancel = context.WithCancel(context.WithoutCancel(ctx))

	// The bus reads Name once, on its first Send, Subscribe or Close, and never again.
	k.bus.Name = k.meterName()

	// Stored last, after every field above is written. Publish and Unsubscribe read the state without
	// taking any lock, so this store is what publishes the kernel to them: anything set after it could
	// be read by a Publish that already saw runningState. bus.Name is the one that bites, since a
	// racing Publish drives the bus's one-time init and would read Name while this goroutine wrote it.
	k.state.Store(runningState)
	k.subsMu.Unlock()

	for _, m := range k.modules {
		if err := m.Init(ctx, k); err != nil {
			return k.startFailed(ctx, permanent(fmt.Errorf("initializing module %s: %w: %w", m.Name(), err, ErrModuleFailed)))
		}
	}
	for _, m := range k.modules {
		if err := m.Start(ctx); err != nil {
			return k.startFailed(ctx, permanent(fmt.Errorf("starting module %s: %w: %w", m.Name(), err, ErrModuleFailed)))
		}
	}
	return nil
}

// meterName is the metrics namespace this kernel actually uses, which is Name or defaultName when Name is
// unset. It exists so the default lives in one place rather than being inlined where it is handed to the
// bus. It is not called name() because k.name and k.Name would then differ by one character.
func (k *Kernel[T]) meterName() string {
	if k.Name == "" {
		return defaultName
	}
	return k.Name
}

// startFailed tears down whatever earlier modules managed to register before startup failed, so a
// half-started kernel does not keep delivering to those subscriptions. Start failure is terminal and the
// teardown enforces it: it closes the bus, which can never reopen, and moves the kernel to stoppedState so
// it can neither be restarted nor go on accepting Register, Publish and Subscribe calls.
func (k *Kernel[T]) startFailed(ctx context.Context, err error) error {
	k.teardown(ctx)
	return err
}

// Register a module within the kernel. The module's name must be unique. This can only be called
// before the kernel is started. Any registration error will panic.
func (k *Kernel[T]) Register(m Module[T]) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.frozen.Load() {
		panic("cannot register new modules after Start: the module set is frozen")
	}

	if m == nil {
		panic("cannot register a nil module")
	}
	if m.Name() == "" {
		panic("cannot register a module with an empty name")
	}

	// n is small, so we don't need a map.
	for _, exist := range k.modules {
		if exist.Name() == m.Name() {
			panic("cannot register two modules with the same name: " + m.Name())
		}
	}
	k.modules = append(k.modules, m)
}

// Registry returns the names of every registered module. The result is immutable, so it can be held and
// read without copying and without any risk of one module's changes being seen by another. This should not
// be called until the kernel has been started or inside a Module's Init or Start method.
func (k *Kernel[T]) Registry() immutable.Map[string, struct{}] {
	if k.frozen.Load() {
		return k.registry
	}
	panic("kernel Registry called before Start: the module set is not frozen yet")
}

func (k *Kernel[T]) freezeReg() {
	m := make(map[string]struct{}, len(k.modules))
	for _, mod := range k.modules {
		m[mod.Name()] = struct{}{}
	}

	// Frozen before any module runs, so a module calling Registry from Init or Start sees the full set.
	// Register is refused from here on, so the map this wraps can never change again.
	k.registry = immutable.NewMap(m)
	k.frozen.Store(true)
}

// Subscribe allows a module to subscribe to a topic. The module will receive messages published to that topic.
// The kernel must have started (subscribing from a module's Init or Start method is fine). Multiple modules
// can subscribe to the same topic; a module subscribing to a topic it is already subscribed to is a no-op.
//
// The handler runs with the context passed to Publish, not the subscription's lifetime. Unsubscribe and Stop
// stop future deliveries but do not cancel a handler that is already running: it ends when it returns or when
// its Publish context is canceled, so a handler that must not run forever must honor that context.
func (k *Kernel[T]) Subscribe(topic string, m Module[T], h Handler[T]) error {
	// Checked before the lifecycle, so a bad topic is caught whatever state the kernel is in rather than
	// being masked by a not-running error on one call and panicking on the next.
	mustValidTopic(topic)

	if m == nil || m.Name() == "" || h == nil {
		return permanent(fmt.Errorf("module and handler must both be set: %w", ErrInvalidArg))
	}

	name := m.Name()
	// Keyed by the canonical topic so dedup and Unsubscribe agree with the bus, which treats "/topic" and
	// "topic" as one topic. Keying by the raw string would let one module subscribe to both spellings and
	// receive every message twice, and leave Unsubscribe unable to find the other spelling's entry.
	key := canonicalTopic(topic)

	k.subsMu.Lock()
	defer k.subsMu.Unlock()

	if k.state.Load() != runningState {
		return permanent(fmt.Errorf("cannot subscribe topic %s: %w", topic, ErrStoppedOrNotStarted))
	}

	lookupKey := subLookup{key: key, name: name}
	if k.subs == nil {
		k.subs = map[subLookup]context.CancelFunc{}
	} else {
		if _, ok := k.subs[lookupKey]; ok {
			return nil
		}
	}

	subCtx, cancel := context.WithCancel(k.baseCtx)

	// The bus subscription is registered before the delivery loop starts. The bus buffers values for a
	// subscription that has been handed out but not yet ranged, so no message sent in this window is lost.
	seq, err := k.bus.Subscribe(subCtx, libTopic(key))
	if err != nil {
		cancel()
		// A teardown that got to the bus first surfaces as the bus's own ErrClosed. Classified as the
		// stopped kernel it is, so one condition returns one error wherever it is observed.
		if errors.Is(err, subscriber.ErrClosed) {
			return permanent(fmt.Errorf("subscribe topic %s (%w): %w", topic, err, ErrStoppedOrNotStarted))
		}
		return permanent(fmt.Errorf("subscribe topic %s (%w): %w", topic, err, ErrBus))
	}

	// The handler is called with the publisher's context (carried in the envelope), not this delivery
	// task's context, so a span, deadline or request-scoped value on the Publish call reaches the handler.
	err = context.Tasks(subCtx).Once(subCtx, "kernelSubscription", func(_ context.Context) error {
		for env := range seq {
			if err := h(env.ctx, env.topic, env.data); err != nil {
				context.Log(env.ctx).Error("kernel subscription handler failed", "module", name, "topic", env.topic, "error", err)
			}
		}
		return nil
	})
	if err != nil {
		cancel()
		// A subCtx that is already done means the lifetime context was cancelled while this call ran, which
		// is the stopped kernel rather than a task-manager failure. Cancellation reaches subCtx through the
		// context tree without taking subsMu, so the teardown handshake above does not catch this one.
		if errors.Is(err, context.Canceled) {
			return permanent(fmt.Errorf("subscribe topic %s (%w): %w", topic, err, ErrStoppedOrNotStarted))
		}
		// Not a bus failure either: Once otherwise refuses only once the background task manager is shut
		// down, so pointing an operator at CatBus would send them to the wrong subsystem.
		return permanent(fmt.Errorf("subscribe topic %s: starting delivery (%w): %w", topic, err, ErrBus))
	}

	k.subs[lookupKey] = cancel
	return nil
}

// Unsubscribe allows a module to unsubscribe from a topic. It is fire-and-forget: it cancels the
// subscription and returns without waiting for the delivery loop to drain, so the module may still receive
// a message or two that was already in flight before the cancellation takes effect. A handler that is
// already running is not canceled; it finishes on its own Publish context.
func (k *Kernel[T]) Unsubscribe(topic string, m Module[T]) {
	// Checked first, for the reason given in Subscribe: topic validity does not depend on the lifecycle.
	mustValidTopic(topic)

	if k.state.Load() != runningState {
		return // Nothing to unsubscribe from before Start, and teardown already cancelled everything.
	}
	if m == nil || m.Name() == "" {
		return
	}

	name := m.Name()
	key := canonicalTopic(topic) // Must match how Subscribe keyed it, whichever spelling was used there.

	k.subsMu.Lock()
	defer k.subsMu.Unlock()

	cancel, ok := k.subs[subLookup{key: key, name: name}]
	if !ok {
		return
	}
	cancel()
	delete(k.subs, subLookup{key: key, name: name})
}

// Publish sends a message to all modules subscribed to the specified topic, including wildcard (*) subscribers.
// It returns an error if the kernel has not started. Publishing to a topic that no one is subscribed to is not
// an error; the message is dropped.
func (k *Kernel[T]) Publish(ctx context.Context, topic string, data T) error {
	// A bare * is a subscription-only pattern, not a topic to publish to. Like any other invalid topic it
	// panics rather than returning an error, but with a message specific to the * misuse. Both checks come
	// before the lifecycle one, for the reason given in Subscribe.
	if topic == "*" {
		panic("cannot publish to topic *: it is a subscribe-only wildcard, publish to a concrete topic")
	}
	mustValidTopic(topic)

	if k.state.Load() != runningState {
		return permanent(fmt.Errorf("cannot publish topic %s: %w", topic, ErrStoppedOrNotStarted))
	}

	// A teardown landing between the check above and here surfaces as the bus's own ErrClosed rather than
	// the kernel's ErrStopped, so it is reported as the same error: one condition, one error, permanent
	// either way.
	if err := k.bus.Send(ctx, topic, envelope[T]{ctx: ctx, topic: topic, data: data}); err != nil {
		if errors.Is(err, subscriber.ErrClosed) {
			return permanent(fmt.Errorf("publish topic %s (%w): %w", topic, err, ErrStoppedOrNotStarted))
		}
		return permanent(fmt.Errorf("publish topic %s (%w): %w", topic, err, ErrBus))
	}
	return nil
}

// Stop ends every subscription and closes the message bus. After Stop the kernel is dead calls to
// methods will generally panic or error.
// Unsubscribe, it does not cancel a handler that is already running; such a handler finishes on its own
// Publish context.
//
// Stop is safe to call more than once, but it is not a completion barrier. The first caller performs the
// teardown; any concurrent or later caller returns as soon as the kernel is marked stopped, which may be
// before the first has finished cancelling subscriptions and closing the bus. Every call does guarantee
// that the kernel refuses further work from the moment it returns.
func (k *Kernel[T]) Stop(ctx context.Context) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.teardown(ctx)
}

// teardown moves the kernel to stoppedState, cancels every live subscription and closes the message bus.
// It is safe to call more than once.
func (k *Kernel[T]) teardown(ctx context.Context) {
	k.subsMu.Lock()
	// Swapped under subsMu and before the snapshot so that a Subscribe racing this teardown either takes
	// subsMu first.
	if k.state.Swap(stoppedState) == stoppedState {
		k.subsMu.Unlock()
		return
	}
	subs := k.subs
	k.subs = nil
	baseCancel := k.baseCancel
	k.subsMu.Unlock()

	for _, cancel := range subs {
		cancel()
	}

	if baseCancel != nil {
		baseCancel()
	}

	k.bus.Close(ctx)
}

// libTopic maps a kernel topic to the subscriber library pattern. The kernel's * (all topics) is the
// library's ** (zero or more segments). Every other topic passes through unchanged.
func libTopic(topic string) string {
	if topic == "*" {
		return "**"
	}
	return topic
}

// canonicalTopic is the form the subscriber library keys a topic by. A leading / is optional and is not
// part of a topic's identity there, so "/a/b" and "a/b" are the same topic; the kernel keys its own
// subscription registry the same way so dedup and Unsubscribe agree with the bus.
func canonicalTopic(topic string) string {
	return strings.TrimPrefix(topic, "/")
}

// The reasons a metrics namespace can be rejected. They are sentinels rather than messages built at the
// point of failure so a test in this package can tell them apart with errors.Is instead of reading text.
// They are wrapped, not flattened, into the error Start returns, so errors.Is reaches them from there too.
// They stay unexported because the reason is not something a caller outside the package acts on: the whole
// class is ErrInvalidArg, which is exported.
var (
	errNameWildcard      = errors.New("a bare * is a subscription pattern, not a name")
	errNameTrailingSlash = errors.New("must not end in /")
	errNameEmptySegment  = errors.New("must not contain an empty segment (//)")
	errNamePattern       = errors.New("must match " + topicRE.String())
)

// nameValid reports why s is unusable as a Kernel's metrics namespace, or nil if it is fine. Empty is valid
// and means the default. It deliberately does not reuse topicValid outright: that accepts a bare *, which is
// a subscription pattern and meaningless as a meter name, and would namespace every metric under "*".
//
// It returns the reason rather than a bool so the rejection names the rule that was broken. A caller told
// only to "match the topic pattern" would be misled: a trailing / and an empty segment both match the
// pattern and are still rejected.
func nameValid(s string) error {
	switch {
	case s == "":
		return nil
	case s == "*":
		return errNameWildcard
	case strings.HasSuffix(s, "/"):
		return errNameTrailingSlash
	case strings.Contains(s, "//"):
		return errNameEmptySegment
	case !topicRE.MatchString(s):
		return errNamePattern
	}
	return nil
}

// mustValidTopic panics unless topic is one the kernel accepts. An invalid topic is a programmer error
// rather than a runtime condition, which is why every entry point panics on one instead of returning an
// error, and why they all check it before anything about the kernel's own state.
func mustValidTopic(topic string) {
	if !topicValid(topic) {
		panic(fmt.Sprintf("invalid topic %q, must be * or match %s with no trailing / or empty segment", topic, topicRE))
	}
}

var topicRE = regexp.MustCompile(`^[A-Za-z0-9/_.-]+$`)

// It is a hand-rolled scan rather than topicRE.MatchString because every Publish, Subscribe and Unsubscribe
// pays for it: the regexp plus the two strings calls it replaces cost around 100ns, which was 70% of a
// publish that has no subscribers. The scan is a third of that. topicRE remains the spec this must agree
// with, and TestTopicValidMatchesRegexp asserts they agree over an exhaustive corpus.
func topicValid(topic string) bool {
	if topic == "*" {
		return true
	}
	if topic == "" {
		return false
	}
	// One pass covers both rules. A trailing / or a // both leave an empty segment that the bus rejects;
	// treat them as invalid here so every path taken with a bad topic panics, rather than some erroring and
	// some panicking. A single leading / is fine: it is optional and not part of the canonical topic, and it
	// is what prevSlash starting false allows. Bytes rather than runes is what topicRE's ASCII-only class
	// means anyway: every byte of a multi-byte rune is >= 0x80, so it falls to the default and is rejected.
	prevSlash := false
	for i := 0; i < len(topic); i++ {
		switch c := topic[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
			prevSlash = false
		case c == '/':
			if prevSlash || i == len(topic)-1 {
				return false
			}
			prevSlash = true
		default:
			return false
		}
	}
	return true
}
