// Package event provides a simple event handling system with subscription and emission capabilities.
package event

import (
	"sync"
)

type Event any
type subscribers map[*Subscription]func(data any)

// Subscription allows unsubscribing from an event.
type Subscription struct {
	event Event
}

// Handler manages event subscriptions and emissions.
type Handler struct {
	subscribers map[Event]subscribers
	mu          sync.RWMutex
}

func NewHandler() *Handler {
	return &Handler{
		subscribers: make(map[Event]subscribers),
	}
}

// Subscribe registers a callback for the given event key.
// Returns a Subscription for later unsubscription.
func (eh *Handler) Subscribe(event Event, callback func(data any)) *Subscription {
	eh.mu.Lock()
	defer eh.mu.Unlock()
	if eh.subscribers[event] == nil {
		eh.subscribers[event] = make(subscribers)
	}
	sub := &Subscription{event: event}
	eh.subscribers[event][sub] = callback
	return sub
}

// Unsubscribe removes the given subscription.
func (eh *Handler) Unsubscribe(sub *Subscription) {
	eh.mu.Lock()
	defer eh.mu.Unlock()
	if subs, ok := eh.subscribers[sub.event]; ok {
		delete(subs, sub)
		if len(subs) == 0 {
			delete(eh.subscribers, sub.event)
		}
	}
}

// Emit notifies all subscribers of the event, passing event data.
// Callbacks are invoked asynchronously in separate goroutines.
func (eh *Handler) Emit(event Event, data any) {
	go eh.emit(event, data)
}

func (eh *Handler) emit(event Event, data any) {
	eh.mu.RLock()
	defer eh.mu.RUnlock()
	if subs, ok := eh.subscribers[event]; ok {
		for _, cb := range subs {
			go cb(data)
		}
	}
}
