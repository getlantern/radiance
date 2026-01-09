package common

import (
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/events"
)

// Device is a machine registered to a user account (e.g. an Android phone or a Windows desktop).
type Device struct {
	ID   string
	Name string
}

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	SetData(*protos.LoginResponse)
	Locale() string
	CountryCode() string
	AccountType() string
	IsPro() bool
	SetEmail(string) error
	GetEmail() string
	Devices() ([]Device, error)
}

type UserChangeEvent struct {
	events.Event
	Old *protos.LoginResponse
	New *protos.LoginResponse
}
