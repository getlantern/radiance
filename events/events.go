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
	"sync"
)

type Event interface {
	// IsEvent is a marker method for the Event interface; it has no runtime use.
	IsEvent()
}

var (
	subscriptions   = make(map[any]map[*Subscription[Event]]func(any))
	subscriptionsMu sync.RWMutex
)

// Subscription allows unsubscribing from an event.
type Subscription[T Event] struct{}

// Subscribe registers a callback function for the given event type T. Returns a Subscription handle
// that can be used to unsubscribe later.
func Subscribe[T Event](callback func(evt T)) *Subscription[T] {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	var evt T
	if subscriptions[evt] == nil {
		subscriptions[evt] = make(map[*Subscription[Event]]func(any))
	}
	sub := &Subscription[T]{}
	subscriptions[evt][(*Subscription[Event])(sub)] = func(e any) { callback(e.(T)) }
	return sub
}

// SubscribeOnce registers a callback function for the given event type T that will be invoked only
// once. Returns a Subscription handle that can be used to unsubscribe if needed.
func SubscribeOnce[T Event](callback func(evt T)) *Subscription[T] {
	var sub *Subscription[T]
	Subscribe(func(evt T) {
		callback(evt)
		sub.Unsubscribe()
	})
	return sub
}

// Unsubscribe removes the given subscription.
func Unsubscribe[T Event](sub *Subscription[T]) {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	var evt T
	if subs, ok := subscriptions[evt]; ok {
		delete(subs, (*Subscription[Event])(sub))
		if len(subs) == 0 {
			delete(subscriptions, evt)
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
	var e T
	if subs, ok := subscriptions[e]; ok {
		for _, cb := range subs {
			go cb(evt)
		}
	}
}
