# ServiceKernel

A lightweight, message-passing kernel for building modular Go services with pub/sub communication between components.

## Overview

ServiceKernel provides a framework for building services as a collection of independent modules that communicate through message passing. It implements a publish/subscribe pattern where modules can subscribe to topics and receive messages asynchronously.

## Key Features

- **Module System**: Build applications as a collection of independent, reusable modules
- **Topic-Based Messaging**: Publish/subscribe pattern with direct topic matching and wildcard support (`*`)
- **Concurrent Message Handling**: Messages fan out to subscribers concurrently, delivered in order per subscriber
- **Type-Safe**: Generic implementation ensures type safety for message passing
- **Lifecycle Management**: Ordered initialization and startup of modules with proper context cancellation

Message routing is backed by [github.com/gostdlib/concurrency/broadcast/subscriber](https://pkg.go.dev/github.com/gostdlib/concurrency/broadcast/subscriber).

## Why

For certain types of microservices, a kernel based structure can make maintenance of the code base much easier.  Like any code structure, this is not a magic bullet for every type of work.  This is not a hammer and everything is a nail.

This has been used to make a service that takes multiple event streams from different sources and writes them into a topic where interested listeners can act on those events.

This structure is perfect for that kind of workflow as adding a source is simply a module that publishes and the handler is simply a module that listens and acts on certain event types and ignores others.

Think about how your program would work and then decide if this is a fit.

## Installation

```bash
go get github.com/johnsiilver/servicekernel
```

## Core Concepts

### Modules

Modules are the building blocks of a ServiceKernel application. Each module must implement the `Module[T]` interface:

```go
type Module[T any] interface {
    // Name returns a unique identifier for the module
    Name() string

    // Init initializes the module with kernel API access
    Init(ctx context.Context, api API[T]) error

    // Start starts the module's operations
    Start(ctx context.Context) error
}
```

### Topics

Topics are string identifiers used for message routing. The kernel supports:
- **Direct matching**: Exact topic match (e.g., `/system/health`). A leading `/` is optional and is not part of a topic's identity, so `/system/health` and `system/health` are the same topic
- **Wildcard matching**: The special topic `*` receives all messages. `*` is subscribe-only — publishing to `*` panics
- **Pattern validation**: Topics must match `^[A-Za-z0-9/_.-]+$` (unless `*`), and may not end in `/` or contain an empty segment (`//`). An invalid topic panics

Best practice is to use hierarchical paths like `/module/feature/event`.

### Messages

Messages are the data passed between modules. The generic type parameter `T` defines the message type for your kernel instance.

### Handlers

Handlers process messages delivered to subscribed topics:

```go
type Handler[T any] func(ctx context.Context, topic string, data T) error
```

A Handler that returns an error has that error logged by the kernel; it does not stop the subscription, which continues to receive later messages. The error return also lets wrappers, as you will see below, do work on behalf of multiple different module handlers.

Handlers subscribed to the same topic are delivered to in the order messages were published (per subscriber). Publishing to a topic that no one is subscribed to is not an error; the message is simply dropped.

The handler is invoked with the context passed to `Publish`, so a trace span, deadline, or request-scoped value on the publishing call flows through to the handler.

Like Go http.HandleFunc, you can use wrappers to do general things for module handlers:

```go
func MetricWrapper[T any](h servicekernel.Handler[T]) servicekernel.Handler[T] {
  return func(ctx context.Context, topic string, data T) error {
  	// This uses a custom Context package, so you won't find context.Meter() in the stdlib.
    context.Meter(ctx).Int64Counter(topic).Add(ctx, 1)
    context.Meter(ctx).Int64UpDownCounter(topic+"-current").Add(ctx, 1)
    defer context.Meter(ctx).Int64UpDownCounter(topic+"-current").Add(ctx, -1)

    err := h(ctx, topic, data)
    if err != nil {
        context.Meter(ctx).Int64Counter(topic+"-errors").Add(ctx, 1)
        return err
    }
    context.Meter(ctx).Int64Counter(topic+"-success").Add(ctx, 1)
  }
}
```

## What should be in a module vs a package?

Not everything needs to be a module. It is absolutely fine to have libraries that get mounted and called. I tend to have modules for top level functionality.  This is things where requests to do something would be mapped or background processes that need to run would be spawned. So, storage for example is something I keep in a package. But if I need a job to run to do maintenance in storage, that might be a module.

Don't overcomplicate your life by making everything a module.

## Example project structure

```
/project
└── service
    ├── spaces
    │   ├── kernel
    │   │   ├── kernel_test.go
    │   │   ├── kernel.go
    │   │   ├── modules
    │   │   │   ├── module1
    │   │   │   │   ├── module1.go
    │   │   │   ├── modules.go
    │   │   └── msgs
    │   │       ├── msgs.go
    │   └── user
    │       ├── grpc
    │       │   ├── grpcserver.go
    │       │   ├── proto
    │       └── syscall
    │           └── syscall.go
```

In this example, you have 2 spaces:

* kernel - Where you kernel wrapper and modules for the kernel are defined
* user - Where connectivity to the outside is defined

`user/` contains a `syscall` package. This isn't required, but I define functions that wrap kernel.API to make calls easier than dealing with message passing directly all the time.

`modules.go` is where I define a common set of args that can be used in constructors for all modules. Usually these have various `interface` types that can provide real or fake clients.

The `msgs/` package is where I define my discriminated union types that act as my generic argument to the `Kernel` type. Again, you can use non-concrete types like `any`, but I find the so called "fat struct" to be superior.

## Usage Example

Here are some basic examples to give an idea of things you can do. These may not be the most efficient way to do these operations, but simply examples to show how things are done and various uses.

### Basic Setup

This illustrates a simple module that can respond to ready requests in order to illustrate a module implemenation.

```go
package main

import (
    "context"
    "log"

    "github.com/johnsiilver/servicekernel/spaces/kernel"
)

const(
	Name = "/path/to/module/health"
	TopicHealthReadyReq = "/path/to/module/health/req"
)

//go:generate stringer -type=MsgType
type MsgType  uint8

const (
	UnknownMsg MsgType = 0 // Unknown
	ReadyMsg MsgType = 1 // Ready
)

// Define your message type
type Message struct {
  Type    string
  Ready ReadyMsg
}

func (m *Message) validate(ctx context.Context) error {
	if m.Type < 1 || m.Type > 1 {
		return fmt.Errorf("invalide Message.Type(%v)", m.Type)
	}
	switch m.Type {
	case ReadyMsg:
		return m.Ready.validate(err)
	}
}

type ReadyMsg {
	Resp chan bool
}

func (r *ReadyMsg) validate(ctx context.Context) error {
	if cap(r.Resp) != 1 {
		return fmt.Errorf("ReadyMsg.Resp must have Resp channel with capactiy of 1")
	}

// Create a simple module
type HealthModule struct {
    api kernel.API[Message]
    ready bool
}

func (h *HealthModule) Name() string {
  return Name
}

func (h *HealthModule) Init(ctx context.Context, api kernel.API[Message]) error {
  h.api = api
  // Subscribe to health check requests
  if err := api.Subscribe(TopicHealthReadyReq, h, h.handleHealthCheck); err != nil {
    return err
  }
  return nil
}

func (h *HealthModule) Start(ctx context.Context) error {
	// We don't need to start anything, so this just returns nil.
	return nil
}

func (h *HealthModule) handleHealthCheck(ctx context.Context, topic string, msg Message) error {
	// Filter out messages we don't care about.
	if msg.Type != ReadyMsg {
		return nil
	}

	if err := msg.validate(ctx); err != nil {
		log.Println(err)
		return nil
	}

	select {
	case msg.Ready.Resp <- h.ready:
	case <-ctx.Done():
		log.Println("context expired while waiting for health check")
	}
	return nil
}

func main() {
    ctx := context.Background()

    // Create kernel
    k := &kernel.Kernel[Message]{}

    // Register modules. Register returns nothing: everything it refuses is a wiring mistake and panics.
    health := &HealthModule{}
    k.Register(health)

    // Start kernel
    if err := k.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

## Advanced Features

### Handler Wrapping

Handlers can be wrapped in other handlers. This can allow you to selectively apply certain function calls across similar or all module handlers. This allows for generic capture of counts, token buckets, circuit breakers, ...  This is similar to how the `net/http` package can wrap `HandleFunc`.

Just remember generic handlers need to be fast or spin off goroutines because they are sitting on top of all messages moving through the kernel. You don't want something to block everything.

```go
package main

import (
    "context"
    "fmt"
    "log"
    "sync/atomic"
    "time"

    "github.com/johnsiilver/servicekernel/spaces/kernel"
)

// rate limiting handler with token bucket
type rateLimiter struct {
	tokens     atomic.Int64
	maxTokens  int64
	refillRate time.Duration
	lastRefill atomic.Int64 // Unix nano timestamp
	h servicekernel.Handler
}

// NewRateLimiter makes a handler that rate limits topic calls.
func NewRateLimiter(maxTokens int, refillRate time.Duration, h servicekernel.Handler) servicekernel.Handler {
	r := &rateLimiter{
		maxTokens:  int64(maxTokens),
		refillRate: refillRate,
		h: h,
	}
	r.tokens.Store(int64(maxTokens))
	r.lastRefill.Store(time.Now().UnixNano())
	return r.handler
}

func (r *rateLimiter) handler(ctx context.Context, topic string, data Msg) error {
	// Refill tokens
	now := time.Now().UnixNano()
	lastRefill := r.lastRefill.Load()
	elapsed := time.Duration(now - lastRefill)
	tokensToAdd := int64(elapsed / r.refillRate)

	if tokensToAdd > 0 {
		// Try to update lastRefill timestamp
		if r.lastRefill.CompareAndSwap(lastRefill, now) {
			// Add tokens up to max
			for {
				current := r.tokens.Load()
				newTokens := min(r.maxTokens, current+tokensToAdd)
				if r.tokens.CompareAndSwap(current, newTokens) {
					break
				}
			}
		}
	}

	// Try to consume a token
	for {
		current := r.tokens.Load()
		if current <= 0 {
			log.Printf("Rate limit exceeded for topic %s from %s", topic, data.Sender)
			return fmt.Errorf("rate limit exceeded")
		}
		if r.tokens.CompareAndSwap(current, current-1) {
			return r.h(ctx, topic, data)
		}
	}
}

// Audit logging handler.
func NewAuditHandler(h servicekernel.Handler) servicekernel.Handler {
	return func(ctx context.Context, topic string, data Msg) error {
		// Log with structured data for audit trail
		log.Printf("AUDIT: timestamp=%s topic=%s sender=%s type=%s",
			time.Now().Format(time.RFC3339), topic, data.Sender, data.Type)
		return h(ctx, topic, data)
	}
}
```

## Best Practices

1. **Topic Naming**: Use hierarchical paths that clearly indicate the message purpose prepended by the module path
   - Good: `/path/to/module/cleanup`
   - Bad: `cleanup`

2. **Module Independence**: Modules should not directly call each other via functions
   - Use message passing for all inter-module communication
   - Inject external dependencies through constructor
   - Should check on other relied on modules during Start() via the Registry that the module exists:
     `if _, ok := api.Registry().Get("other/module"); !ok { return fmt.Errorf(...) }`

3. **Error Handling**:
   - Return errors from Init() and Start() to fail fast
   - Include error channels in messages for async error reporting

4. **Context Usage**:
   - Always respect context cancellation
   - Use context for tracing and logging

5. **Message Design**:
   - Use a discriminated union pattern for message types
   - Include sender identification for diagnostics
   - Validate messages before processing

## Performance Considerations

- Message routing is handled by `github.com/gostdlib/concurrency/broadcast/subscriber`, which stores each value once per topic no matter how many subscribers it has
- Each subscription runs its own delivery loop, so a slow subscriber does not hold up publishers, but its backlog can build memory
- Wildcard (`*`) subscribers receive all messages - use sparingly

## Lifecycle

- `Register` must be called before `Start`. It returns nothing: a nil module, an empty or duplicate name, and registering after `Start` are all wiring mistakes and panic
- `Subscribe` returns an error; the kernel must be started before subscribing (subscribing from a module's `Init`/`Start` is fine)
- `Unsubscribe` is fire-and-forget: it cancels the subscription and returns immediately, so the module may still receive a message or two that was already in flight before the cancellation takes effect
- `Unsubscribe` and `Stop` stop *future* deliveries but do not cancel a handler that is already running — the handler runs on its `Publish` context, so it ends when it returns or when that context is canceled (a handler that must not run forever must honor that context)
- `Stop(ctx)` ends every subscription and closes the message bus; after `Stop`, `Publish` returns an error
- A kernel is **single use**. Once it has stopped — via `Stop` or a failed `Start` — it cannot be restarted, and a new `Kernel` must be created. The underlying message bus cannot reopen once closed, so this is enforced rather than merely advised. `Subscribe` and `Publish` return an error on a dead kernel; `Start` and `Register` panic, since calling either on a finished kernel is a wiring mistake
- **`Stop` is the only thing that ends a kernel.** Cancelling the context passed to `Start` does not: that context supplies values and a span to the modules, and honouring its cancellation is theirs to do. A kernel therefore outlives its `Start` context, and a caller that never calls `Stop` leaks its subscriptions' delivery loops

## Errors

Every error the kernel returns wraps one of a small set of sentinels, so callers match on identity rather
than on message text:

```go
if errors.Is(err, kernel.ErrStoppedOrNotStarted) {
  // The kernel is dead; build a new one.
}
```

| Error | Condition |
|---|---|
| `ErrStoppedOrNotStarted` | A call needing a running kernel, made before `Start` or after the kernel stopped |
| `ErrInvalidArg` | A missing or malformed argument |
| `ErrModuleFailed` | A module returned an error from `Init` or `Start` |
| `ErrBus` | The message bus or the background task manager refused an operation |

Every one of these also matches `errors.ErrPermanent`, so a caller running a kernel call under
`retry/exponential` stops rather than retrying something that can never succeed.

Conditions a caller cannot recover from at all — starting a kernel twice, registering a duplicate module
name, an invalid topic — panic rather than returning an error.

If `Start` returns an error, treat that kernel as dead and build a new one. Some failures happen before the
kernel has built anything and would technically leave it startable, but that is not a guarantee — do not
rely on it.

## Metrics

The message bus records OTel metrics under a meter namespaced by the kernel's `Name` field, which defaults
to `"kernel"`. Set `Name` before `Start` only if a process runs more than one kernel and their metrics need
to be told apart; it must satisfy the same character rules as a topic except that a bare `*` is not a name,
and `Start` rejects it with the reason if it does not.
Writing `Name` concurrently with `Start` is a data race, not merely a change that gets ignored.

## Dependencies

The kernel uses libraries from `github.com/gostdlib/base` and `github.com/gostdlib/concurrency`:
- `base/context`: Enhanced context with goroutine pools and background tasks
- `base/concurrency/sync`: Thread-safe data structures
- `base/errors`: Error helpers and the `ErrPermanent` marker
- `base/values/immutable`: Immutable `Map` returned by `Registry()`
- `concurrency/broadcast/subscriber`: Topic/path based message broadcast

## License

See LICENSE file in the repository.
