package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	runtimeDebug "runtime/debug"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/radiance/internal"
)

type selection struct {
	GroupTag    string `json:"groupTag"`
	OutboundTag string `json:"outboundTag"`
}

// SelectOutbound selects an outbound within a group.
func SelectOutbound(ctx context.Context, groupTag, outboundTag string) error {
	_, err := sendRequest[empty](ctx, "POST", selectEndpoint, selection{groupTag, outboundTag})
	return err
}

func (s *Server) selectHandler(w http.ResponseWriter, r *http.Request) {
	if s.service.Status() != StatusRunning {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	var p selection
	err := json.NewDecoder(r.Body).Decode(&p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			http.Error(w, fmt.Sprint(r), http.StatusInternalServerError)
		}
	}()
	slog.Log(nil, internal.LevelTrace, "selecting outbound", "group", p.GroupTag, "outbound", p.OutboundTag)
	outbound, err := getGroupOutbound(s.service.Ctx(), p.GroupTag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selector, isSelector := outbound.(Selector)
	if !isSelector {
		http.Error(w, fmt.Sprintf("outbound %q is not a selector", p.GroupTag), http.StatusBadRequest)
		return
	}
	slog.Log(nil, internal.LevelTrace, "setting outbound", "outbound", p.OutboundTag)
	if !selector.SelectOutbound(p.OutboundTag) {
		http.Error(w, fmt.Sprintf("outbound %q not found in group", p.OutboundTag), http.StatusBadRequest)
		return
	}
	cs := s.service.ClashServer()
	if mode := cs.Mode(); mode != p.GroupTag {
		slog.Log(nil, internal.LevelDebug, "changing clash mode", "new", p.GroupTag, "old", mode)
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
func GetSelected(ctx context.Context) (group, tag string, err error) {
	res, err := sendRequest[selection](ctx, "GET", selectEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	return res.GroupTag, res.OutboundTag, nil
}

func (s *Server) selectedHandler(w http.ResponseWriter, r *http.Request) {
	if s.service.Status() != StatusRunning {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	mode := cs.Mode()
	selector, err := getGroupOutbound(s.service.Ctx(), mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := selection{
		GroupTag:    mode,
		OutboundTag: selector.Now(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// GetActiveOutbound retrieves the outbound that is actively being used, resolving nested groups
// if necessary.
func GetActiveOutbound(ctx context.Context) (group, tag string, err error) {
	res, err := sendRequest[selection](ctx, "GET", activeEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	return res.GroupTag, res.OutboundTag, nil
}

func (s *Server) activeOutboundHandler(w http.ResponseWriter, r *http.Request) {
	if s.service.Status() != StatusRunning {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	mode := cs.Mode()
	group, err := getGroupOutbound(s.service.Ctx(), mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
