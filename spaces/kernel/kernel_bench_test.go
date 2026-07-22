package kernel

import (
	"strconv"
	"testing"

	"github.com/gostdlib/base/concurrency/sync"
	"github.com/gostdlib/base/context"
)

// benchKernel builds a started kernel with n modules registered and returns it along with the modules in
// registration order. Modules have distinct names so they can each hold an independent subscription to the
// same topic. It wraps startKernel, which tests and benchmarks share, and only adds the ordered slice.
func benchKernel(b *testing.B, n int) (*Kernel[string], []*fakeModule) {
	b.Helper()

	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = "m" + strconv.Itoa(i)
	}
	k, byName := startKernel(b, b.Context(), names...)

	mods := make([]*fakeModule, n)
	for i, name := range names {
		mods[i] = byName[name]
	}
	return k, mods
}

// BenchmarkPublishFanout measures delivery to n subscribers, drained each iteration, for both exact-topic
// and wildcard (*) subscriptions. The publish topic is always "topic"; wildcard subscribers match it via **.
func BenchmarkPublishFanout(b *testing.B) {
	scenarios := []struct {
		name     string
		subTopic string
	}{
		{name: "exact", subTopic: "topic"},
		{name: "wildcard", subTopic: "*"},
	}

	for _, s := range scenarios {
		for _, n := range []int{1, 10, 100} {
			b.Run(s.name+"/"+strconv.Itoa(n), func(b *testing.B) {
				ctx := b.Context()
				k, mods := benchKernel(b, n)

				done := make(chan struct{}, n)
				h := func(ctx context.Context, topic, data string) error {
					done <- struct{}{}
					return nil
				}
				for i := 0; i < n; i++ {
					doSubscribe(b, k, s.subTopic, mods[i], h)
				}

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := k.Publish(ctx, "topic", "data"); err != nil {
						b.Fatalf("BenchmarkPublishFanout: publish: %v", err)
					}
					for j := 0; j < n; j++ {
						<-done
					}
				}
			})
		}
	}
}

// BenchmarkPublishNoSubscribers measures publishing to a topic no one is subscribed to, while a
// subscriber exists on a different topic. The result is ignored, as the point is the routing-miss
// cost, not the outcome.
func BenchmarkPublishNoSubscribers(b *testing.B) {
	ctx := b.Context()
	k, mods := benchKernel(b, 1)

	doSubscribe(b, k, "other", mods[0], func(ctx context.Context, topic, data string) error { return nil })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k.Publish(ctx, "topic", "data")
	}
}

// BenchmarkPublishParallel measures send-side throughput under concurrent publishers. Delivery is
// not drained per op, so this measures the publish/send cost, which is the fair cross-implementation
// comparison under contention.
func BenchmarkPublishParallel(b *testing.B) {
	ctx := b.Context()
	k, mods := benchKernel(b, 1)

	doSubscribe(b, k, "topic", mods[0], func(ctx context.Context, topic, data string) error { return nil })

	// Failures are recorded rather than reported from inside RunParallel: its body runs on goroutines other
	// than the one running the benchmark, and b.Fatalf there calls FailNow off the benchmark goroutine,
	// which is not allowed and would not stop the run cleanly.
	var failed sync.AtomicValue[bool]

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := k.Publish(ctx, "topic", "data"); err != nil {
				failed.Store(true)
				return
			}
		}
	})
	b.StopTimer()
	if failed.Load() {
		b.Fatalf("BenchmarkPublishParallel: publish returned an error")
	}
}

// BenchmarkSubscribeUnsubscribe measures the cost of a subscription lifecycle (subscribe then
// unsubscribe) on a fixed topic and module.
func BenchmarkSubscribeUnsubscribe(b *testing.B) {
	k, mods := benchKernel(b, 1)
	h := func(ctx context.Context, topic, data string) error { return nil }

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doSubscribe(b, k, "topic", mods[0], h)
		k.Unsubscribe("topic", mods[0])
	}
}
