package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/getlantern/radiance/traces"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing/service"
	"go.opentelemetry.io/otel"
)

// GetGroups retrieves the list of group outbounds.
func GetGroups() ([]OutboundGroup, error) {
	return sendRequest[[]OutboundGroup]("GET", groupsEndpoint, nil)
}

func (s *Server) groupHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.groupHandler")
	defer span.End()
	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, errServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	groups, err := getGroups(s.service.Ctx())
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(groups); err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// OutboundGroup represents a group of outbounds.
type OutboundGroup struct {
	Tag      string
	Type     string
	Selected string
	ItemList []OutboundGroupItem
}

// OutboundGroupItem represents outbounds within a group.
type OutboundGroupItem struct {
	Tag  string
	Type string

	// URLTestTime and URLTestDelay are only available for URLTest outbounds.
	URLTestTime  int64
	URLTestDelay int32
}

func getGroups(ctx context.Context) ([]OutboundGroup, error) {
	historyStorage := service.PtrFromContext[urltest.HistoryStorage](ctx)
	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
	if outboundMgr == nil {
		return nil, errors.New("outbound manager not found")
	}
	outbounds := outboundMgr.Outbounds()
	var iGroups []adapter.OutboundGroup
	for _, it := range outbounds {
		if group, isGroup := it.(adapter.OutboundGroup); isGroup {
			iGroups = append(iGroups, group)
		}
	}
	var groups []OutboundGroup
	for _, iGroup := range iGroups {
		group := OutboundGroup{
			Tag:      iGroup.Tag(),
			Type:     iGroup.Type(),
			Selected: iGroup.Now(),
		}
		for _, itemTag := range iGroup.All() {
			itemOutbound, isLoaded := outboundMgr.Outbound(itemTag)
			if !isLoaded {
				continue
			}

			item := OutboundGroupItem{
				Tag:  itemTag,
				Type: itemOutbound.Type(),
			}
			if history := historyStorage.LoadURLTestHistory(adapter.OutboundTag(itemOutbound)); history != nil {
				item.URLTestTime = history.Time.Unix()
				item.URLTestDelay = int32(history.Delay)
			}
			group.ItemList = append(group.ItemList, item)
		}
		groups = append(groups, group)
	}
	return groups, nil
}
