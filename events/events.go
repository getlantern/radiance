// Package events provides a simple publish-subscribe mechanism for event handling.
//
// This package does not define specific events; instead, publishers define their own event types
// by embedding the Event interface in their structs. Subscribers can subscribe to these custom
// events by providing callback functions that accept the event type as a parameter.
//
// Example:
//
// package somepkg
//
//	type SomeEvent struct {
//	    events.Event // embedding marks this as an event type
//	    Message string
//	}
//
//	func doSomething() {
//		events.Emit(SomeEvent{Message: "hello world"})
//	}
//
// package other
//
//	func doOtherthing() {
//		sub := events.Subscribe(func(evt somepkg.SomeEvent) {
//		    fmt.Println("Received event:", evt.Message)
//		})
//	}
package events

import (
	"context"
	stdlog "log"
	"reflect"
	"sync"
	"sync/atomic"
)

type Event interface {
	// IsEvent is a marker method for the Event interface; it has no runtime use.
	IsEvent()
}

var (
	subscriptions   = make(map[reflect.Type]map[*Subscription[Event]]func(any))
	subscriptionsMu sync.RWMutex
)

// Subscription allows unsubscribing from an event.
type Subscription[T Event] struct {
	_ byte // padding to avoid empty struct optimizations
}

// Subscribe registers a callback function for the given event type T. Returns a Subscription handle
// that can be used to unsubscribe later.
func Subscribe[T Event](callback func(evt T)) *Subscription[T] {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	key := reflect.TypeFor[T]()
	if subscriptions[key] == nil {
		subscriptions[key] = make(map[*Subscription[Event]]func(any))
	}
	sub := &Subscription[T]{}
	subscriptions[key][(*Subscription[Event])(sub)] = func(e any) { callback(e.(T)) }
	return sub
}

// SubscribeOnce registers a callback function for the given event type T that will be invoked only
// once. Returns a Subscription handle that can be used to unsubscribe if needed.
func SubscribeOnce[T Event](callback func(evt T)) *Subscription[T] {
	return SubscribeUntil(callback, func(evt T) bool { return true })
}

// SubscribeUntil registers a callback function for the given event type T that will be invoked until
// the provided condition function returns true for an event. Returns a Subscription handle that can
// be used to unsubscribe if needed.
func SubscribeUntil[T Event](callback func(evt T), cond func(evt T) bool) *Subscription[T] {
	var done atomic.Bool
	var sub *Subscription[T]
	sub = Subscribe(func(evt T) {
		if done.Load() {
			return
		}
		callback(evt)
		if cond(evt) {
			done.Store(true)
			sub.Unsubscribe()
		}
	})
	return sub
}

// SubscribeContext registers a callback for event type T that is automatically unsubscribed when
// the provided context is cancelled.
func SubscribeContext[T Event](ctx context.Context, callback func(evt T)) *Subscription[T] {
	sub := Subscribe(callback)
	go func() {
		<-ctx.Done()
		sub.Unsubscribe()
	}()
	return sub
}

// Unsubscribe removes the given subscription.
func Unsubscribe[T Event](sub *Subscription[T]) {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	key := reflect.TypeFor[T]()
	if subs, ok := subscriptions[key]; ok {
		delete(subs, (*Subscription[Event])(sub))
		if len(subs) == 0 {
			delete(subscriptions, key)
		}
	}
}

func (e *Subscription[T]) Unsubscribe() {
	Unsubscribe(e)
}

// Emit notifies all subscribers of the event, passing event data. Callbacks are invoked
// asynchronously in separate goroutines.
func Emit[T Event](evt T) {
	subscriptionsMu.RLock()
	defer subscriptionsMu.RUnlock()
	key := reflect.TypeFor[T]()
	subs, ok := subscriptions[key]
	// Diagnostic: surfaces the subscriber count at emit time so a missing
	// FlutterEvent on the consumer side is distinguishable from "no
	// subscribers registered for this type" vs "subscribers registered
	// but callback panics silently." Spam-friendly when traffic spikes,
	// but we're investigating a zero-callback path so the noise is
	// short-lived; remove (or downgrade to Debug) once the chain works.
	emitDebugLogger(key, len(subs))
	if !ok {
		return
	}
	for _, cb := range subs {
		go cb(evt)
	}
}

// emitDebugLogger is a package-level var so tests can suppress the
// per-emit log, and so prod can swap in slog. Default uses Go's stdlib
// log so events package doesn't need to import slog (and avoid a cycle
// with anything that imports events for its own log forwarding).
var emitDebugLogger = func(key reflect.Type, subCount int) {
	stdlog.Printf("events.Emit type=%s subscribers=%d", key, subCount)
}
