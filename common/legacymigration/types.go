// Package legacymigration binds legacy identity adoption to an enrolled Windows user.
package legacymigration

import (
	"encoding/json"
	"errors"
	"runtime"
)

// Request carries the original user's legacy credentials and supported preferences.
type Request struct {
	Version         int    `json:"version"`
	MigrationID     string `json:"migration_id"`
	SourceDirectory string `json:"source_directory"`
	UserID          int64  `json:"user_id"`
	Token           string `json:"token"`
	DeviceID        string `json:"device_id"`
	Locale          string `json:"locale"`
	AutoReport      bool   `json:"auto_report"`
	ProxyAll        bool   `json:"proxy_all"`
	AutoLaunch      bool   `json:"auto_launch"`
}

// Receipt identifies the accepted request without exposing its credentials.
type Receipt struct {
	Version       int    `json:"version"`
	MigrationID   string `json:"migration_id"`
	RequestSHA256 string `json:"request_sha256"`
	UserID        int64  `json:"user_id"`
	DeviceID      string `json:"device_id"`
	UserLevel     string `json:"user_level"`
	SourceSID     string `json:"source_sid"`
	Status        string `json:"status"`
}

// State is the live backend state used to determine migration readiness.
type State struct {
	UserID                                    int64
	Token, DeviceID, Locale, UserLevel        string
	AutoReport, ProxyAll, AutoLaunch, Healthy bool
}

// Record is protected machine state; its request and user data contain credentials.
type Record struct {
	Version         int             `json:"version"`
	MigrationID     string          `json:"migration_id"`
	SourceSID       string          `json:"source_sid"`
	SourceDirectory string          `json:"source_directory"`
	Status          string          `json:"status"`
	RequestSHA256   string          `json:"request_sha256,omitempty"`
	Request         *Request        `json:"request,omitempty"`
	Receipt         *Receipt        `json:"receipt,omitempty"`
	UserData        json.RawMessage `json:"user_data,omitempty"`
}

// Store keeps enrollment records in administrator-controlled storage.
type Store interface {
	Load() (*Record, error)
	// Save atomically persists a complete record and returns only after it is durable.
	Save(*Record) error
}

// ErrConflict means enrollment, destination, or backend state is incompatible with the request.
var ErrConflict = errors.New("legacy migration conflicts with existing state")

// ErrUnauthorized means the caller lacks required privileges or a verified, enrolled identity.
var ErrUnauthorized = errors.New("legacy migration is not authorized")

// Load returns the protected enrollment, or nil when no enrollment exists.
func Load() (*Record, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	store, err := OpenStore()
	if err != nil {
		return nil, err
	}
	record, err := store.Load()
	if err != nil || record == nil {
		return record, err
	}
	return record, validateRecord(record)
}

// OwnerSID reports the authorized user only after durable adoption completes.
func OwnerSID() string {
	record, err := Load()
	if err != nil || record == nil || record.Status != "completed" {
		return ""
	}
	return record.SourceSID
}
