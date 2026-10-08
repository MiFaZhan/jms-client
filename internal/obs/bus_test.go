package obs

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitTimeout bounds a wait for an asynchronous delivery. Delivery is
// deliberately asynchronous (DESIGN.md「可观测性」), so a test must wait for the
// observable effect rather than assume it happened inside Publish.
const waitTimeout = 5 * time.Second

// collecting is a Subscriber that records every event it receives.
type collecting struct {
	ch chan Event
}

func newCollecting() *collecting { return &collecting{ch: make(chan Event, 256)} }

func (c *collecting) Publish(e Event) { c.ch <- e }

// take returns the next event, failing the test if none arrives in time.
func (c *collecting) take(t *testing.T) Event {
	t.Helper()
	select {
	case e := <-c.ch:
		return e
	case <-time.After(waitTimeout):
		t.Fatal("no event delivered within the timeout")
		return Event{}
	}
}

// drain returns everything already queued, without waiting.
func (c *collecting) drain() []Event {
	var out []Event
	for {
		select {
		case e := <-c.ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestPublishReachesEverySubscriber(t *testing.T) {
	bus := NewBus(8)
	subs := []*collecting{newCollecting(), newCollecting(), newCollecting()}
	for _, s := range subs {
		bus.Subscribe(s)
	}

	bus.Publish(Event{Kind: KindExecStart, Command: "one"})
	bus.Publish(Event{Kind: KindExecEnd, Command: "two"})

	for i, s := range subs {
		first := s.take(t)
		second := s.take(t)
		if first.Command != "one" || first.Kind != KindExecStart {
			t.Errorf("subscriber %d: first event = %+v, want command one kind %s", i, first, KindExecStart)
		}
		if second.Command != "two" || second.Kind != KindExecEnd {
			t.Errorf("subscriber %d: second event = %+v, want command two kind %s", i, second, KindExecEnd)
		}
	}
}

func TestPublishIsNonBlockingAndCountsDroppedEvents(t *testing.T) {
	// The ring is large enough to retain every event the test publishes, so
	// the replay assertion below measures the ring, not its capacity.
	bus := NewBus(256)

	// The subscriber parks on release, so the drain goroutine is stuck inside
	// Subscriber.Publish exactly as a stalled observer would be.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	bus.Subscribe(SubscriberFunc(func(Event) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}))
	t.Cleanup(func() { close(release) })

	bus.Publish(Event{Kind: KindExecStart, Command: "blocker"})
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("the subscriber never received the first event")
	}

	// Publishing must return even though the subscriber is parked.
	// subscriberBuffer more events fill the queue; the next one has nowhere to
	// go and must be counted as dropped.
	const published = 1 + subscriberBuffer + 1
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < published-1; i++ {
			bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("fill-%d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("Publish blocked on a stuck subscriber")
	}

	if got := bus.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1 (queue holds %d events)", got, subscriberBuffer)
	}
	// A dropped delivery must not lose the replay record.
	if got := len(bus.Last(published)); got != published {
		t.Errorf("Last() returned %d events, want %d: drops must not affect the ring", got, published)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus(8)
	kept := newCollecting()
	removed := newCollecting()
	bus.Subscribe(kept)
	bus.Subscribe(removed)

	bus.Publish(Event{Kind: KindExecStart, Command: "before"})
	if got := kept.take(t).Command; got != "before" {
		t.Fatalf("kept subscriber received %q, want before", got)
	}
	if got := removed.take(t).Command; got != "before" {
		t.Fatalf("removed subscriber received %q, want before", got)
	}

	bus.Unsubscribe(removed)
	bus.Publish(Event{Kind: KindExecStart, Command: "after"})
	if got := kept.take(t).Command; got != "after" {
		t.Fatalf("kept subscriber received %q, want after", got)
	}
	// The removed slot's queue was empty when it was closed, so nothing more
	// can reach it.
	if got := removed.drain(); len(got) != 0 {
		t.Errorf("unsubscribed subscriber still received %d event(s): %+v", len(got), got)
	}
}

func TestUnsubscribeLeavesOtherSubscribersRegistered(t *testing.T) {
	bus := NewBus(8)
	a, b, c := newCollecting(), newCollecting(), newCollecting()
	bus.Subscribe(a)
	bus.Subscribe(b)
	bus.Subscribe(c)

	bus.Unsubscribe(b)
	if len(bus.slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(bus.slots))
	}

	bus.Publish(Event{Kind: KindExecStart, Command: "x"})
	if a.take(t).Command != "x" || c.take(t).Command != "x" {
		t.Error("the remaining subscribers did not both receive the event")
	}
}

func TestUnsubscribeUnknownSubscriberIsANoop(t *testing.T) {
	bus := NewBus(8)
	a, stranger := newCollecting(), newCollecting()
	bus.Subscribe(a)

	bus.Unsubscribe(stranger)
	bus.Publish(Event{Kind: KindExecStart, Command: "x"})
	if a.take(t).Command != "x" {
		t.Error("the registered subscriber stopped receiving events")
	}
}

func TestUnsubscribeMatchesFuncSubscribers(t *testing.T) {
	// A function-typed interface value is not comparable, so Unsubscribe must
	// not panic and must still find the registration.
	bus := NewBus(8)
	received := make(chan Event, 4)
	fn := SubscriberFunc(func(e Event) { received <- e })
	bus.Subscribe(fn)
	bus.Unsubscribe(fn)

	if len(bus.slots) != 0 {
		t.Fatalf("slots = %d, want 0", len(bus.slots))
	}
	bus.Publish(Event{Kind: KindExecStart, Command: "x"})
	select {
	case e := <-received:
		t.Errorf("unsubscribed function subscriber received %+v", e)
	default:
	}
}

func TestLastReturnsMostRecentOldestFirst(t *testing.T) {
	bus := NewBus(8)
	for i := 0; i < 5; i++ {
		bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("c%d", i)})
	}

	got := bus.Last(2)
	if len(got) != 2 {
		t.Fatalf("Last(2) returned %d events, want 2", len(got))
	}
	if got[0].Command != "c3" || got[1].Command != "c4" {
		t.Errorf("Last(2) = %q, %q; want c3, c4 (oldest first)", got[0].Command, got[1].Command)
	}
}

func TestLastHandlesNBeyondTheRecordedCount(t *testing.T) {
	bus := NewBus(8)
	bus.Publish(Event{Kind: KindExecStart, Command: "only"})

	got := bus.Last(100)
	if len(got) != 1 || got[0].Command != "only" {
		t.Errorf("Last(100) = %+v, want the single recorded event", got)
	}
}

func TestLastHandlesNBeyondTheRingSize(t *testing.T) {
	bus := NewBus(3)
	for i := 0; i < 3; i++ {
		bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("c%d", i)})
	}
	got := bus.Last(100)
	if len(got) != 3 {
		t.Fatalf("Last(100) on a full ring returned %d events, want 3", len(got))
	}
	if got[0].Command != "c0" || got[2].Command != "c2" {
		t.Errorf("Last(100) = %q..%q, want c0..c2", got[0].Command, got[2].Command)
	}
}

func TestLastIsEmptyOnAFreshBus(t *testing.T) {
	if got := NewBus(4).Last(10); len(got) != 0 {
		t.Errorf("Last on an empty bus returned %+v", got)
	}
	if got := NewBus(4).Last(0); got != nil {
		t.Errorf("Last(0) = %+v, want nil", got)
	}
}

func TestRingWrapsAndPreservesOrder(t *testing.T) {
	bus := NewBus(3)
	for i := 0; i < 7; i++ {
		bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("c%d", i)})
	}

	got := bus.Last(3)
	if len(got) != 3 {
		t.Fatalf("Last(3) returned %d events, want 3", len(got))
	}
	for i, want := range []string{"c4", "c5", "c6"} {
		if got[i].Command != want {
			t.Fatalf("Last(3)[%d] = %q, want %q", i, got[i].Command, want)
		}
	}
	// A second full pass over the ring must not resurrect overwritten events.
	bus.Publish(Event{Kind: KindExecStart, Command: "c7"})
	got = bus.Last(3)
	for i, want := range []string{"c5", "c6", "c7"} {
		if got[i].Command != want {
			t.Errorf("after wrapping twice, Last(3)[%d] = %q, want %q", i, got[i].Command, want)
		}
	}
}

func TestNewBusUsesDefaultRingSize(t *testing.T) {
	bus := NewBus(0)
	for i := 0; i < DefaultRingSize+10; i++ {
		bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("c%d", i)})
	}
	got := bus.Last(DefaultRingSize + 10)
	if len(got) != DefaultRingSize {
		t.Fatalf("ring holds %d events, want DefaultRingSize (%d)", len(got), DefaultRingSize)
	}
	if got[0].Command != "c10" {
		t.Errorf("oldest retained event = %q, want c10", got[0].Command)
	}
}

func TestLastIsACopy(t *testing.T) {
	bus := NewBus(4)
	bus.Publish(Event{Kind: KindExecStart, Command: "original"})
	got := bus.Last(1)
	got[0].Command = "mutated"
	if again := bus.Last(1); again[0].Command != "original" {
		t.Errorf("Last() exposed the ring buffer: got %q", again[0].Command)
	}
}

func TestConcurrentSubscribeUnsubscribeAndPublish(t *testing.T) {
	// Subscribe/Unsubscribe must be safe while Publish runs, and must not
	// corrupt the ring or the subscriber list.
	const subRounds = 200
	const pubRounds = 200
	bus := NewBus(64)
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < subRounds; j++ {
				s := newCollecting()
				bus.Subscribe(s)
				bus.Unsubscribe(s)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < pubRounds; j++ {
				bus.Publish(Event{Kind: KindExecStart, Command: fmt.Sprintf("g%d-%d", n, j)})
			}
		}(i)
	}

	wg.Wait()

	if got := len(bus.Last(1000)); got != 64 {
		t.Errorf("ring holds %d events after the race, want 64", got)
	}
	if len(bus.slots) != 0 {
		t.Errorf("slots = %d after the race, want 0", len(bus.slots))
	}
}

func TestNilBusIsInert(t *testing.T) {
	// Callers treat a nil bus as "publication disabled" (mcpserver.Options,
	// cli.Runtime), so no method may panic on a nil receiver.
	var bus *Bus
	bus.Publish(Event{Kind: KindExecStart})
	bus.Subscribe(newCollecting())
	bus.Unsubscribe(newCollecting())
	if got := bus.Last(5); got != nil {
		t.Errorf("nil bus Last() = %+v, want nil", got)
	}
	if got := bus.Dropped(); got != 0 {
		t.Errorf("nil bus Dropped() = %d, want 0", got)
	}
}

func TestZeroBusStillReplays(t *testing.T) {
	// A Bus built as a struct literal (never through NewBus) must still work:
	// a library package may not panic on a usable-looking value.
	bus := &Bus{}
	bus.Publish(Event{Kind: KindExecStart, Command: "first"})
	bus.Publish(Event{Kind: KindExecStart, Command: "second"})

	got := bus.Last(2)
	if len(got) != 2 || got[0].Command != "first" || got[1].Command != "second" {
		t.Errorf("Last(2) = %+v, want first then second", got)
	}
}
