package common

import (
	"github.com/getlantern/radiance/events"
)

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	//SetData(*protos.LoginResponse) error
	//GetData() (*protos.LoginResponse, error)
	Locale() string
	SetLocale(string)
	CountryCode() string
	AccountType() string
	IsPro() bool
}

type UserChangeEvent struct {
	events.Event
	Old UserInfo
	New UserInfo
}
