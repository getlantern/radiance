package api

import (
	"net/http"

	"github.com/getlantern/radiance/common"
)

// APIHandler is a struct that contains the API clients for User and Pro.
type APIHandler struct {
	User      *User
	ProServer *Pro
}

func NewAPIHandlerInternal(httpClient *http.Client, userinfo common.UserInfo) *APIHandler {
	u := NewUser(httpClient, userinfo)
	pro := NewPro(httpClient, userinfo)
	return &APIHandler{
		User:      u,
		ProServer: pro,
	}
}
