package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
)

// GetGroups retrieves the list of group outbounds.
func GetGroups(ctx context.Context) ([]OutboundGroup, error) {
	return sendRequest[[]OutboundGroup](ctx, "GET", groupsEndpoint, nil)
}

func (s *Server) groupHandler(w http.ResponseWriter, r *http.Request) {
	if s.service.Status() != Connected {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	groups, err := getGroups(s.service.Ctx())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(groups); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// OutboundGroup represents a group of outbounds.
type OutboundGroup struct {
	Tag       string
	Type      string
	Selected  string
	Outbounds []Outbounds
}

// Outbounds represents outbounds within a group.
type Outbounds struct {
	Tag  string
	Type string
}

func getGroups(ctx context.Context) ([]OutboundGroup, error) {
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

			item := Outbounds{
				Tag:  itemTag,
				Type: itemOutbound.Type(),
			}
			group.Outbounds = append(group.Outbounds, item)
		}
		groups = append(groups, group)
	}
	return groups, nil
}
