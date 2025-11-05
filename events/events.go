// Package events provides a simple publish-subscribe mechanism for event handling.
package events

import (
	"sync"
)

type Event comparable

var (
	subscriptions   = make(map[any]map[*Subscription[any]]func(any))
	subscriptionsMu sync.RWMutex
)

// Subscription allows unsubscribing from an event.
type Subscription[T Event] struct{}

func Subscribe[T Event](callback func(evt T)) *Subscription[T] {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	var evt T
	if subscriptions[evt] == nil {
		subscriptions[evt] = make(map[*Subscription[any]]func(any))
	}
	sub := &Subscription[T]{}
	subscriptions[evt][(*Subscription[any])(sub)] = func(e any) { callback(e.(T)) }
	return sub
}

// Unsubscribe removes the given subscription.
func Unsubscribe[T Event](sub *Subscription[T]) {
	subscriptionsMu.Lock()
	defer subscriptionsMu.Unlock()
	var evt T
	if subs, ok := subscriptions[evt]; ok {
		delete(subs, (*Subscription[any])(sub))
		if len(subs) == 0 {
			delete(subscriptions, evt)
		}
	}
}

// Emit notifies all subscribers of the event, passing event data.
// Callbacks are invoked asynchronously in separate goroutines.
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
