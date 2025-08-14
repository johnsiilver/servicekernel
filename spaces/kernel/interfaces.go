package kernel

import (
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/values/generics/sets"
)

// Handler is a function type that handles messages published to a topic.
type Handler[T any] func(ctx context.Context, topic string, data T)

// Interceptor is a function type that can intercept messages as they are published to a topic
// before they reach the handlers. If cont is false, the message will not be published and will
// not go to any other Interceptors. However, Publish will not return an error. If err is not nil,
// the same thing will happen, but Publish will return an error.
// If a message needs a response, the Interceptor is responsible for that.
// Be careful with Interceptors, as they are executed in order and unlike messages sent to module handlers,
// this is executed in a single goroutine. Interceptors with long-running operations can muck up performance.
// Interceptors can use the * topic, which will intercept all messages. This should only be done if the Interceptor
// needs to manipulate all messages or deny messages from being published. Otherwise, it is better to have a module
// that listens on all topics.
type Interceptor[T any] func(ctx context.Context, topic string, symbols Symbols[T], data T) (cont bool, err error)

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
