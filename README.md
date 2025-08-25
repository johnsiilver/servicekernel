# ServiceKernel

A lightweight, message-passing kernel for building modular Go services with pub/sub communication between components.

## Overview

ServiceKernel provides a framework for building services as a collection of independent modules that communicate through message passing. It implements a publish/subscribe pattern where modules can subscribe to topics and receive messages asynchronously.

## Key Features

- **Module System**: Build applications as a collection of independent, reusable modules
- **Topic-Based Messaging**: Publish/subscribe pattern with direct topic matching and wildcard support (`*`)
- **Message Interception**: Interceptors can process or filter messages before they reach subscribers
- **Concurrent Message Handling**: Messages are distributed to handlers concurrently using goroutine pools
- **Type-Safe**: Generic implementation ensures type safety for message passing
- **Lifecycle Management**: Ordered initialization and startup of modules with proper context cancellation

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
- **Direct matching**: Exact topic string match (e.g., `/system/health`)
- **Wildcard matching**: The special topic `*` receives all messages
- **Pattern validation**: Topics must match `^[A-Za-z0-9/_.-]+$` (unless `*`)

Best practice is to use hierarchical paths like `/module/feature/event`.

### Messages

Messages are the data passed between modules. The generic type parameter `T` defines the message type for your kernel instance.

### Handlers

Handlers process messages delivered to subscribed topics:

```go
type Handler[T any] func(ctx context.Context, topic string, data T)
```

### Interceptors

Interceptors can inspect, modify, or block messages before delivery:

```go
type Interceptor[T any] func(ctx context.Context, topic string, api API[T], data T) (cont bool, err error)
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
  api.Subscribe(TopicHealthReadyReq, h, h.handleHealthCheck)
  return nil
}

func (h *HealthModule) Start(ctx context.Context) error {
	// We don't need to start anything, so this just returns nil.
	return nil
}

func (h *HealthModule) handleHealthCheck(ctx context.Context, topic string, msg Message) {
	// Filter out messages we don't care about.
	if msg.Type != ReadyMsg {
		return
	}

	if err := msg.validate(ctx); err != nil {
		log.Println(err)
		return
	}

	select {
	case msg.Ready.Resp <- h.ready:
	case <-ctx.Done():
		log.Println("context expired while waiting for health check")
	}
}

func main() {
    ctx := context.Background()

    // Create kernel
    k := &kernel.Kernel[Message]{}

    // Register modules
    health := &HealthModule{}
    if err := k.Register(health); err != nil {
        log.Fatal(err)
    }

    // Start kernel
    if err := k.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

### Real-World Example: Event Processing System

Here's a more complex example showing an event processing system with multiple modules:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/johnsiilver/servicekernel/spaces/kernel"
)

//go:generate stringer -type=MsgType
type MsgType uint8

const (
    UnknownMsg MsgType = 0
    EventMsg   MsgType = 1
    MetricMsg  MsgType = 2
    AlertMsg   MsgType = 3
)

// Message type with multiple message variants. This is a `fat struct` implementation that avoids heap allocations
// and determines the message type via `Type`. However, you can use `any` instead of `Msg` as your generic type and
// do a more traditional type assertion methodology.
type Msg struct {
    Type   MsgType
    Sender string

    Event  EventData
    Metric MetricData
    Alert  AlertData
}

func (m *Msg) Validate(ctx context.Context) error {
    if m.Sender == "" {
        return fmt.Errorf("message sender cannot be empty")
    }

    switch m.Type {
    case EventMsg:
        return m.Event.Validate(ctx)
    case MetricMsg:
        return m.Metric.Validate(ctx)
    case AlertMsg:
        return m.Alert.Validate(ctx)
    default:
        return fmt.Errorf("invalid Message.Type(%v)", m.Type)
    }
}

type EventData struct {
    ID      string
    Level   string // "info", "warning", "critical"
    Source  string
    Message string
    Time    time.Time
}

func (e *EventData) Validate(ctx context.Context) error {
    if e.Source == "" {
        return fmt.Errorf("event source cannot be empty")
    }
    if e.Level != "info" && e.Level != "warning" && e.Level != "critical" {
        return fmt.Errorf("invalid event level: %s", e.Level)
    }
    return nil
}

type MetricData struct {
    Name  string
    Value float64
    Tags  map[string]string
}

func (m *MetricData) Validate(ctx context.Context) error {
    if m.Name == "" {
        return fmt.Errorf("metric name cannot be empty")
    }
    return nil
}

type AlertData struct {
    Severity string
    Message  string
    Source   string
    Resp     chan<- bool // For acknowledgment
}

func (a *AlertData) Validate(ctx context.Context) error {
    if a.Source == "" || a.Message == "" {
        return fmt.Errorf("alert source and message cannot be empty")
    }
    return nil
}

const EventProcessorName = "/services/events/processor"
const TopicAllEvents = "*"
const TopicCriticalEvents = "/events/critical"
const TopicAlerts = "/alerts/send"

type EventProcessor struct {
    api         kernel.API[Msg]
    // This is inefficient, but fine for an example.
    mu sync.Mutex
    eventCount  map[string]int
}

func NewEventProcessor() *EventProcessor {
    return &EventProcessor{
        eventCount: make(map[string]int),
    }
}

func (e *EventProcessor) Name() string {
    return EventProcessorName
}

func (e *EventProcessor) Init(ctx context.Context, api kernel.API[Msg]) error {
    e.api = api
    // Subscribe to all events for logging
    api.Subscribe(TopicAllEvents, e, e.handleAllEvents)
    // Subscribe to critical events for alerting
    api.Subscribe(TopicCriticalEvents, e, e.handleCriticalEvents)
    return nil
}

func (e *EventProcessor) Start(ctx context.Context) error {
    log.Printf("[%s] Started event processor", e.Name())
    return nil
}

func (e *EventProcessor) handleAllEvents(ctx context.Context, topic string, msg Msg) {
    // Log all events for audit purposes
    if msg.Type == EventMsg {
    	e.mu.Lock()
     	e.eventCount[msg.Event.Level]++
      e.mu.Unlock()
      log.Printf("[%s] Event from %s: [%s] %s", e.Name(), msg.Event.Source, msg.Event.Level, msg.Event.Message)
    }
}

func (e *EventProcessor) handleCriticalEvents(ctx context.Context, topic string, msg Msg) {
    if msg.Type != EventMsg || msg.Event.Level != "critical" {
        return
    }

    // Handle critical events by creating alerts
    log.Printf("[%s] CRITICAL EVENT: %s - %s", e.Name(), msg.Event.Source, msg.Event.Message)

    // Send alert to alert manager
    e.api.Publish(ctx, TopicAlerts, Msg{
        Type:   AlertMsg,
        Sender: e.Name(),
        Alert: AlertData{
            Severity: "high",
            Message:  fmt.Sprintf("Critical event: %s", msg.Event.Message),
            Source:   msg.Event.Source,
        },
    })
}

// Metrics collector module
const MetricsCollectorName = "/services/metrics/collector"
const TopicMetricsReport = "/metrics/report"
const TopicMetricsQuery = "/metrics/query"

type MetricsCollector struct {
    api     kernel.API[Msg]
    mu sync.Mutex
    metrics map[string][]float64
}

func NewMetricsCollector() *MetricsCollector {
    return &MetricsCollector{
        metrics: make(map[string][]float64),
    }
}

func (m *MetricsCollector) Name() string {
    return MetricsCollectorName
}

func (m *MetricsCollector) Init(ctx context.Context, api kernel.API[Msg]) error {
    m.api = api
    api.Subscribe(TopicMetricsReport, m, m.handleMetricReport)
    return nil
}

func (m *MetricsCollector) Start(ctx context.Context) error {
	// Start background aggregation
	go m.aggregateMetrics(ctx)
	log.Printf("[%s] Started metrics collector", m.Name())
	return nil
}

func (m *MetricsCollector) handleMetricReport(ctx context.Context, topic string, msg Msg) {
	if msg.Type != MetricMsg {
	    return
	}

	if err := msg.Validate(ctx); err != nil {
	    log.Printf("[%s] Invalid metric: %v", m.Name(), err)
	    return
	}

	mu.Lock()
	defer mu.Unlock()
	m.metrics[msg.Metric.Name] = append(m.metrics[msg.Metric.Name], msg.Metric.Value)

	// Check thresholds and generate events if needed
	if msg.Metric.Name == "error_rate" && msg.Metric.Value > 0.05 {
	    m.api.Publish(ctx, TopicCriticalEvents, Msg{
	        Type:   EventMsg,
	        Sender: m.Name(),
	        Event: EventData{
	            Level:   "critical",
	            Source:  "metrics",
	            Message: fmt.Sprintf("Error rate exceeded threshold: %.2f%%", msg.Metric.Value*100),
	            Time:    time.Now(),
	        },
	    })
	}
}

func (m *MetricsCollector) aggregateMetrics(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
    select {
    case <-ticker.C:
    	mu.Lock()
	    for name, values := range m.metrics {
	        if len(values) > 0 {
	            avg := 0.0
	            for _, v := range values {
	                avg += v
	            }
	            avg /= float64(len(values))
	            log.Printf("[%s] Metric %s: avg=%.2f, count=%d",
	                m.Name(), name, avg, len(values))
	        }
	    }
	    // Clear metrics after aggregation
	    m.metrics = make(map[string][]float64)
	    mu.Unlock()
    case <-ctx.Done():
        return
    }
	}
}

// Alert manager module
const AlertManagerName = "/services/alerts/manager"

type AlertManager struct {
	api    kernel.API[Msg]
	alerts []AlertData
}

func NewAlertManager() *AlertManager {
	return &AlertManager{
	    alerts: make([]AlertData, 0),
	}
}

func (a *AlertManager) Name() string {
	return AlertManagerName
}

func (a *AlertManager) Init(ctx context.Context, api kernel.API[Msg]) error {
	a.api = api
	api.Subscribe(TopicAlerts, a, a.handleAlert)
	return nil
}

func (a *AlertManager) Start(ctx context.Context) error {
	log.Printf("[%s] Started alert manager", a.Name())
	return nil
}

func (a *AlertManager) handleAlert(ctx context.Context, topic string, msg Msg) {
	if msg.Type != AlertMsg {
	    return
	}

	if err := msg.Validate(ctx); err != nil {
	    log.Printf("[%s] Invalid alert: %v", a.Name(), err)
	    return
	}

	a.alerts = append(a.alerts, msg.Alert)
	log.Printf("[%s] ALERT [%s]: %s from %s",
	    a.Name(), msg.Alert.Severity, msg.Alert.Message, msg.Alert.Source)

	// Send acknowledgment if channel provided
	if msg.Alert.Resp != nil {
	    select {
	    case msg.Alert.Resp <- true:
	    case <-ctx.Done():
	    }
	}

	// In production, this would send to external alerting systems
}

// Module registration
func setupKernel(ctx context.Context) (*kernel.Kernel[Msg], error) {
	k := &kernel.Kernel[Msg]{}

	// Register modules
	modules := []kernel.Module[Msg]{
	    NewEventProcessor(),
	    NewMetricsCollector(),
	    NewAlertManager(),
	}

	for _, m := range modules {
	    if err := k.Register(m); err != nil {
	        return nil, fmt.Errorf("failed to register module %v: %w", m.Name(), err)
	    }
	}

	// Add interceptors for cross-cutting concerns
	if *debugFlag {
	   	k.AddInterceptor("*", loggingInterceptor)
	}
	k.AddInterceptor("/events/*", eventValidationInterceptor)

	return k, nil
}

// Logging interceptor
func loggingInterceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
	log.Printf("Message on topic %s from %s (type: %s)", topic, data.Sender, data.Type)
	return true, nil
}

// Event validation interceptor
func eventValidationInterceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
	if data.Type == EventMsg {
    if err := data.Event.Validate(ctx); err != nil {
	    log.Printf("Invalid event on topic %s: %v", topic, err)
	    return false, err
    }
	}
	return true, nil
}

func main() {
    ctx := context.Background()

    k, err := setupKernel(ctx)
    if err != nil {
        log.Fatal(err)
    }

    if err := k.Start(ctx); err != nil {
        log.Fatal(err)
    }

    // Simulate some events
    k.Publish(ctx, TopicCriticalEvents, Msg{
        Type:   EventMsg,
        Sender: "main",
        Event: EventData{
            ID:      "evt-001",
            Level:   "critical",
            Source:  "database",
            Message: "Connection pool exhausted",
            Time:    time.Now(),
        },
    })

    // Simulate metrics
    k.Publish(ctx, TopicMetricsReport, Msg{
        Type:   MetricMsg,
        Sender: "main",
        Metric: MetricData{
            Name:  "error_rate",
            Value: 0.08, // This will trigger an alert
            Tags:  map[string]string{"service": "api", "endpoint": "/users"},
        },
    })
}
```

## Advanced Features

### Message Interception

Add interceptors to implement cross-cutting concerns like logging, validation, and rate limiting:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "sync"
    "time"

    "github.com/johnsiilver/servicekernel/spaces/kernel"
)

// Rate limiting interceptor with token bucket
type RateLimiter struct {
	tokens     atomic.Int64
	maxTokens  int64
	refillRate time.Duration
	lastRefill atomic.Int64 // Unix nano timestamp
}

func NewRateLimiter(maxTokens int, refillRate time.Duration) *RateLimiter {
	r := &RateLimiter{
		maxTokens:  int64(maxTokens),
		refillRate: refillRate,
	}
	r.tokens.Store(int64(maxTokens))
	r.lastRefill.Store(time.Now().UnixNano())
	return r
}

func (r *RateLimiter) Interceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
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
			return false, fmt.Errorf("rate limit exceeded")
		}
		if r.tokens.CompareAndSwap(current, current-1) {
			return true, nil
		}
	}
}

// Audit logging interceptor
func auditInterceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
	// Log with structured data for audit trail
	log.Printf("AUDIT: timestamp=%s topic=%s sender=%s type=%s",
		time.Now().Format(time.RFC3339), topic, data.Sender, data.Type)
	return true, nil
}

// Security validation interceptor
func securityInterceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
	// Check if sender is authorized for this topic
	if topic == "/admin/*" && !isAdminModule(data.Sender) {
		log.Printf("SECURITY: Unauthorized access to %s by %s", topic, data.Sender)
		return false, fmt.Errorf("unauthorized access")
	}

	// Validate message integrity
	if err := data.Validate(ctx); err != nil {
		log.Printf("SECURITY: Invalid message on %s: %v", topic, err)
		return false, err
	}

	return true, nil
}

func isAdminModule(sender string) bool {
	adminModules := map[string]bool{
		"/services/admin":   true,
		"/services/control": true,
	}
	return adminModules[sender]
}

// Circuit breaker interceptor
type CircuitBreaker struct {
    mu           sync.Mutex
    failureCount int
    maxFailures  int
    state        string // "closed", "open", "half-open"
    lastFailTime time.Time
    timeout      time.Duration
}

func NewCircuitBreaker(maxFailures int, timeout time.Duration) *CircuitBreaker {
    return &CircuitBreaker{
        maxFailures: maxFailures,
        state:       "closed",
        timeout:     timeout,
    }
}

func (cb *CircuitBreaker) Interceptor(ctx context.Context, topic string, api kernel.API[Msg], data Msg) (bool, error) {
    cb.mu.Lock()
    defer cb.mu.Unlock()

    // Check circuit state
    switch cb.state {
    case "open":
        if time.Since(cb.lastFailTime) > cb.timeout {
            cb.state = "half-open"
            cb.failureCount = 0
            log.Printf("Circuit breaker: transitioning to half-open for topic %s", topic)
        } else {
            return false, fmt.Errorf("circuit breaker is open")
        }
    case "half-open":
        // Allow one request through to test
    case "closed":
        // Normal operation
    }

    return true, nil
}

func (cb *CircuitBreaker) RecordSuccess() {
    cb.mu.Lock()
    defer cb.mu.Unlock()

    if cb.state == "half-open" {
        cb.state = "closed"
        cb.failureCount = 0
        log.Printf("Circuit breaker: closed")
    }
}

func (cb *CircuitBreaker) RecordFailure() {
    cb.mu.Lock()
    defer cb.mu.Unlock()

    cb.failureCount++
    cb.lastFailTime = time.Now()

    if cb.failureCount >= cb.maxFailures {
        cb.state = "open"
        log.Printf("Circuit breaker: opened after %d failures", cb.failureCount)
    }
}

// Setup with multiple interceptors
func setupWithInterceptors(ctx context.Context) (*kernel.Kernel[Msg], error) {
    k := &kernel.Kernel[Msg]{}

    // Create rate limiter
    rateLimiter := NewRateLimiter(100, time.Second)

    // Create circuit breaker
    circuitBreaker := NewCircuitBreaker(5, 30*time.Second)

    // Add interceptors in order of execution
    // Global interceptors (apply to all messages)
    k.AddInterceptor("*", auditInterceptor)
    k.AddInterceptor("*", rateLimiter.Interceptor)

    // Topic-specific interceptors
    k.AddInterceptor("/admin/*", securityInterceptor)
    k.AddInterceptor("/external/*", circuitBreaker.Interceptor)

    return k, nil
}
```

## Best Practices

1. **Topic Naming**: Use hierarchical paths that clearly indicate the message purpose prepended by the module path
   - Good: `/path/to/module/cleanup`
   - Bad: `cleanup`

2. **Module Independence**: Modules should not directly call each other via functions
   - Use message passing for all inter-module communication
   - Inject external dependencies through constructor
   - Should check on other relied on mondules during Start() via the Registry that the module exists

3. **Error Handling**:
   - Return errors from Init() and Start() to fail fast
   - Use interceptors for centralized error handling for common things like message validation
   - Include error channels in messages for async error reporting

4. **Context Usage**:
   - Always respect context cancellation
   - Use context for tracing and logging

5. **Message Design**:
   - Use a discriminated union pattern for message types
   - Include sender identification for diagnostics
   - Validate messages before processing

## Performance Considerations

- Messages to handlers are dispatched concurrently using goroutine pools
- Topics use sharded maps for concurrent access
- Interceptors run sequentially and should be lightweight
- Wildcard (`*`) subscribers receive all messages - use sparingly

## Dependencies

The kernel uses several libraries from the `github.com/gostdlib/base` package:
- `context`: Enhanced context with goroutine pools
- `concurrency/sync`: Thread-safe data structures
- `values/immutable`: Immutable data types for safe concurrent access
- `values/generics/sets`: Generic set implementation

## License

See LICENSE file in the repository.
