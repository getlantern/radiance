package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	runtimeDebug "runtime/debug"
	"time"

	"github.com/getlantern/radiance/traces"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing/service"
	"go.opentelemetry.io/otel"
)

type selection struct {
	GroupTag    string `json:"groupTag"`
	OutboundTag string `json:"outboundTag"`
}

// SelectOutbound selects an outbound within a group.
func SelectOutbound(groupTag, outboundTag string) error {
	_, err := sendRequest[empty]("POST", selectEndpoint, selection{groupTag, outboundTag})
	return err
}

func (s *Server) selectHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.selectHandler")
	defer span.End()
	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, errServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	var p selection
	err := json.NewDecoder(r.Body).Decode(&p)
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusBadRequest)
		return
	}
	outbound, err := getGroupOutbound(s.service.Ctx(), p.GroupTag)
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	selector, isSelector := outbound.(Selector)
	if !isSelector {
		http.Error(w, traces.RecordError(span, fmt.Errorf("outbound %q is not a selector", p.GroupTag)).Error(), http.StatusBadRequest)
		return
	}
	if !selector.SelectOutbound(p.OutboundTag) {
		http.Error(w, traces.RecordError(span, fmt.Errorf("outbound %q not found in group", p.OutboundTag)).Error(), http.StatusBadRequest)
		return
	}
	cs := s.service.ClashServer()
	if cs.Mode() != p.GroupTag {
		s.service.ClashServer().SetMode(p.GroupTag)
		conntrack.Close()
		go func() {
			time.Sleep(time.Second)
			runtimeDebug.FreeOSMemory()
		}()
	}
	w.WriteHeader(http.StatusOK)
}

// Selector is helper interface to check if an outbound is a selector or wrapper of selector.
type Selector interface {
	adapter.OutboundGroup
	SelectOutbound(tag string) bool
}

// GetSelected retrieves the currently selected outbound and its group.
func GetSelected() (group, tag string, err error) {
	res, err := sendRequest[selection]("GET", selectEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	return res.GroupTag, res.OutboundTag, nil
}

func (s *Server) selectedHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.selectedHandler")
	defer span.End()
	if s.service.Status() == StatusClosed || s.service.Status() == StatusClosing {
		http.Error(w, traces.RecordError(span, fmt.Errorf("service closed")).Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	mode := cs.Mode()
	selector, err := getGroupOutbound(s.service.Ctx(), mode)
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	res := selection{
		GroupTag:    mode,
		OutboundTag: selector.Now(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetActiveOutbound retrieves the outbound that is actively being used, resolving nested groups
// if necessary.
func GetActiveOutbound() (group, tag string, err error) {
	res, err := sendRequest[selection]("GET", activeEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	return res.GroupTag, res.OutboundTag, nil
}

func (s *Server) activeOutboundHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.activeOutboundHandler")
	defer span.End()
	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, errServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	mode := cs.Mode()
	group, err := getGroupOutbound(s.service.Ctx(), mode)
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	tag := group.Now()
	// if the selected outbound is also a group, retrieve its selected outbound
	// continue until we reach a non-group outbound
	for {
		group, err = getGroupOutbound(s.service.Ctx(), tag)
		if err != nil {
			break
		}
		tag = group.Now()
	}
	if tag == "" {
		tag = "unavailable"
	}
	res := selection{
		GroupTag:    mode,
		OutboundTag: tag,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func getGroupOutbound(ctx context.Context, tag string) (adapter.OutboundGroup, error) {
	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
	if outboundMgr == nil {
		return nil, errors.New("outbound manager not found")
	}

	outbound, loaded := outboundMgr.Outbound(tag)
	if !loaded {
		return nil, fmt.Errorf("group not found: %s", tag)
	}
	group, isGroup := outbound.(adapter.OutboundGroup)
	if !isGroup {
		return nil, fmt.Errorf("outbound is not a group: %s", tag)
	}
	return group, nil
}
