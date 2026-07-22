package kernel

import (
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/immutable"
)

// Handler handles messages published to a topic. ctx is the context passed to
// Publish (carrying its span, deadline and request-scoped values), not the subscription's lifetime, so
// Unsubscribe and Stop do not cancel it via ctx.
//
// A returned error is logged with the module and topic that produced it, and delivery continues; it never
// reaches the publisher.
type Handler[T any] func(ctx context.Context, topic string, data T) error

// API allows modules to interact within the kernel.
// The API interface may be extended at any time, so if implementing a fake API for testing, embed
// the API in your struct and implement the methods you need. If not, you can be broken by future changes:
// we will not do a Major version bump for additions to this interface, only for changes that break
// existing functionality.
type API[T any] interface {
	// Registry returns the names of all modules registered in the kernel. This allows a module on Init() or Start()
	// to check if another module is registered. The result is immutable, so holding on to it is safe and cheap.
	// Membership is a Get(): if _, ok := api.Registry().Get("name"); ok { ... }.
	Registry() immutable.Map[string, struct{}]
	// Subscribe allows a module to subscribe to a topic. The module will receive messages published to that topic.
	// It returns ErrStoppedOrNotStarted if the kernel is not running and ErrInvalidArg if the module or
	// handler is missing. An invalid topic panics rather than returning an error, as it does everywhere
	// else in the kernel.
	Subscribe(topic string, m Module[T], h Handler[T]) error
	// Unsubscribe allows a module to unsubscribe from a topic. It is fire-and-forget, so the module may
	// still receive a message or two that was already in flight.
	Unsubscribe(topic string, m Module[T])
	// Publish sends a message to all modules subscribed to the specified topic.
	Publish(ctx context.Context, topic string, data T) error
}

// Module defines the interface that a module must implement.
type Module[T any] interface {
	// Name returns the name of the module. Since names cannot collide, this must be unique.
	// Best practice is to use the package name + the type name as the module name.
	Name() string
	// Init initializes the module with the provided context and kernel. Startup order happens in the order
	// of registration, so if a module Init relies on another module, it should be registered after that
	// module. You should avoid module dependencies via Init as it can lead to circular dependencies.
	//
	// Only the API[T] surface may be called from Init or Start. The kernel holds an internal lock across
	// these calls, so reaching the concrete *Kernel[T] by type assertion and calling Register, Start, ...
	// deadlocks rather than returning an error.
	Init(ctx context.Context, api API[T]) error
	// Start starts the module. This cannot block and Start must honor context cancellation.
	Start(ctx context.Context) error
}
