package kernel

import (
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/generics/sets"
)

// Handler is a function type that handles messages published to a topic.
type Handler[T any] func(ctx context.Context, topic string, data T)

// Symbols provide an interface for modules to interact within the kernel.
// The Symbols interface can be extended at any time. If implementing a fake Symbols for testing, embed
// the Symbols in your struct and implement the methods you need. If not you can be broken by future changes as
// we will not do a Major version bump for additions to this interface, only for changes that break existing functionality.
type Symbols[T any] interface {
	// Registry returns a set of all module names registered in the kernel. This allows a module on Start() to check if
	// another module is registered in the kernel.
	Registry() sets.Set[string]
	// Subscribe allows a module to subscribe to a topic. The module will receive messages published to that topic.
	Subscribe(topic string, m Module[T], h Handler[T])
	// Unsubscribe allows a module to unsubscribe from a topic.
	Unsubscribe(topic string, m Module[T])
	// Publish sends a message to all modules subscribed to the specified topic.
	Publish(ctx context.Context, topic string, data T) error
}

// Module defines the interface that a module must implement.
type Module[T any] interface {
	// Name returns the name of the module. Since names cannot collide, this must be unique.
	// Best practice is to use the package name + the type name as the module name.
	Name() string
	// Init initializes the module with the provided context and kernel. Startup order happens in the order of registration,
	// so if a module Init relies on another module, it should be registered after that module. You should avoid
	// module dependencies via Init as it can lead to circular dependencies.
	Init(ctx context.Context, s Symbols[T]) error
	// Start starts the module. This cannot block and Start must honor context cancellation.
	Start(ctx context.Context) error
}
