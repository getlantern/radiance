package common

import (
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/events"
)

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	SetData(*protos.LoginResponse) error
	GetData() (*protos.LoginResponse, error)
	Locale() string
	SetLocale(string)
	CountryCode() string
	AccountType() string
	IsPro() bool
	SetEmail(string) error
	GetEmail() string
}

type UserChangeEvent struct {
	events.Event
	Old *protos.LoginResponse
	New *protos.LoginResponse
}
