// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/getlantern/radiance (interfaces: httpServer,configHandler)
//
// Generated by this command:
//
//	mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance httpServer,configHandler
//

// Package radiance is a generated GoMock package.
package radiance

import (
	context "context"
	net "net"
	reflect "reflect"

	config "github.com/getlantern/radiance/config"
	gomock "go.uber.org/mock/gomock"
)

// MockhttpServer is a mock of httpServer interface.
type MockhttpServer struct {
	ctrl     *gomock.Controller
	recorder *MockhttpServerMockRecorder
	isgomock struct{}
}

// MockhttpServerMockRecorder is the mock recorder for MockhttpServer.
type MockhttpServerMockRecorder struct {
	mock *MockhttpServer
}

// NewMockhttpServer creates a new mock instance.
func NewMockhttpServer(ctrl *gomock.Controller) *MockhttpServer {
	mock := &MockhttpServer{ctrl: ctrl}
	mock.recorder = &MockhttpServerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockhttpServer) EXPECT() *MockhttpServerMockRecorder {
	return m.recorder
}

// Serve mocks base method.
func (m *MockhttpServer) Serve(listener net.Listener) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Serve", listener)
	ret0, _ := ret[0].(error)
	return ret0
}

// Serve indicates an expected call of Serve.
func (mr *MockhttpServerMockRecorder) Serve(listener any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Serve", reflect.TypeOf((*MockhttpServer)(nil).Serve), listener)
}

// Shutdown mocks base method.
func (m *MockhttpServer) Shutdown(ctx context.Context) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Shutdown", ctx)
	ret0, _ := ret[0].(error)
	return ret0
}

// Shutdown indicates an expected call of Shutdown.
func (mr *MockhttpServerMockRecorder) Shutdown(ctx any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Shutdown", reflect.TypeOf((*MockhttpServer)(nil).Shutdown), ctx)
}

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
func (m *MockconfigHandler) GetConfig(ctx context.Context) ([]*config.ProxyConnectConfig, string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetConfig", ctx)
	ret0, _ := ret[0].([]*config.ProxyConnectConfig)
	ret1, _ := ret[1].(string)
	ret2, _ := ret[2].(error)
	return ret0, ret1, ret2
}

// GetConfig indicates an expected call of GetConfig.
func (mr *MockconfigHandlerMockRecorder) GetConfig(ctx any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetConfig", reflect.TypeOf((*MockconfigHandler)(nil).GetConfig), ctx)
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
