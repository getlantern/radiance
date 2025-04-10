// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/getlantern/radiance (interfaces: configHandler)
//
// Generated by this command:
//
//	mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance configHandler
//

// Package radiance is a generated GoMock package.
package radiance

import (
	context "context"
	reflect "reflect"

	config "github.com/getlantern/common"
	gomock "go.uber.org/mock/gomock"
)

// MockconfigHandler is a mock of configHandler interface.
type MockconfigHandler struct {
	ctrl     *gomock.Controller
	recorder *MockconfigHandlerMockRecorder
	isgomock struct{}
}

// MockconfigHandlerMockRecorder is the mock recorder for MockconfigHandler.
type MockconfigHandlerMockRecorder struct {
	mock *MockconfigHandler
}

// NewMockconfigHandler creates a new mock instance.
func NewMockconfigHandler(ctrl *gomock.Controller) *MockconfigHandler {
	mock := &MockconfigHandler{ctrl: ctrl}
	mock.recorder = &MockconfigHandlerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockconfigHandler) EXPECT() *MockconfigHandlerMockRecorder {
	return m.recorder
}

// GetConfig mocks base method.
func (m *MockconfigHandler) GetConfig(ctx context.Context) (*config.ConfigResponse, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetConfig", ctx)
	ret0, _ := ret[0].(*config.ConfigResponse)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetConfig indicates an expected call of GetConfig.
func (mr *MockconfigHandlerMockRecorder) GetConfig(ctx any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetConfig", reflect.TypeOf((*MockconfigHandler)(nil).GetConfig), ctx)
}

// ListAvailableServers mocks base method.
func (m *MockconfigHandler) ListAvailableServers(ctx context.Context) ([]config.ServerLocation, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ListAvailableServers", ctx)
	ret0, _ := ret[0].([]config.ServerLocation)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ListAvailableServers indicates an expected call of ListAvailableServers.
func (mr *MockconfigHandlerMockRecorder) ListAvailableServers(ctx any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ListAvailableServers", reflect.TypeOf((*MockconfigHandler)(nil).ListAvailableServers), ctx)
}

// SetPreferredServerLocation mocks base method.
func (m *MockconfigHandler) SetPreferredServerLocation(country, city string) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "SetPreferredServerLocation", country, city)
}

// SetPreferredServerLocation indicates an expected call of SetPreferredServerLocation.
func (mr *MockconfigHandlerMockRecorder) SetPreferredServerLocation(country, city any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SetPreferredServerLocation", reflect.TypeOf((*MockconfigHandler)(nil).SetPreferredServerLocation), country, city)
}

// Stop mocks base method.
func (m *MockconfigHandler) Stop() {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Stop")
}

// Stop indicates an expected call of Stop.
func (mr *MockconfigHandlerMockRecorder) Stop() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Stop", reflect.TypeOf((*MockconfigHandler)(nil).Stop))
}
