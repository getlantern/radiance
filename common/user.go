package common

import (
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/events"
)

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	SetData(*protos.LoginResponse)
}

type UserChangeEvent struct {
	events.Event
}
