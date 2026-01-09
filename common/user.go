package common

import (
	"github.com/getlantern/radiance/events"
)

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
}

type UserChangeEvent struct {
	events.Event
}
