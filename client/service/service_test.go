package boxservice

import (
	"context"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
)

func TestUpdateOutbounds(t *testing.T) {
	tests := []struct {
		name      string
		outbounds []adapter.Outbound
		updates   []option.Outbound
		exclude   []string
		want      []adapter.Outbound
		error     bool
	}{
		{
			name: "upsert outbounds",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "tag1", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
				&mockEndpoint{tag: "tag3", typ: "type3"},
			},
			updates: []option.Outbound{
				{Tag: "tag1", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "tag1", typ: "NewType"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "exclude outbounds",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Outbound{
				{Tag: "direct", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			exclude: []string{"direct", "dns"},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "update valid and return error for missing tag",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Outbound{
				{Tag: "tag2", Type: "NewType"},
				{Tag: "", Type: "NewType"},
			},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "tag2", typ: "NewType"},
			},
			error: true,
		},
	}
	for _, tt := range tests {
		ctx := context.Background()
		logger := log.NewNOPFactory()
		t.Run(tt.name, func(t *testing.T) {
			mgr := mockOutboundManager{outbounds: tt.outbounds}
			err := updateOutbounds(ctx, &mgr, nil, logger, tt.updates, tt.exclude)
			if tt.error {
				assert.Error(t, err)
			}
			got := mgr.Outbounds()
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestUpdateEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []adapter.Endpoint
		updates   []option.Endpoint
		exclude   []string
		want      []adapter.Endpoint
		error     bool
	}{
		{
			name: "upsert endpoints",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "tag1", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
				&mockEndpoint{tag: "tag3", typ: "type3"},
			},
			updates: []option.Endpoint{
				{Tag: "tag1", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "tag1", typ: "NewType"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "exclude endpoints",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Endpoint{
				{Tag: "direct", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			exclude: []string{"direct", "dns"},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "update valid and return error for missing tag",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Endpoint{
				{Tag: "tag2", Type: "NewType"},
				{Tag: "", Type: "NewType"},
			},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "tag2", typ: "NewType"},
			},
			error: true,
		},
	}
	for _, tt := range tests {
		ctx := context.Background()
		logger := log.NewNOPFactory()
		t.Run(tt.name, func(t *testing.T) {
			mgr := mockEndpointManager{endpoints: tt.endpoints}
			err := updateEndpoints(ctx, &mgr, nil, logger, tt.updates, tt.exclude)
			if tt.error {
				assert.Error(t, err)
			}
			got := mgr.Endpoints()
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

type mockOutboundManager struct {
	adapter.OutboundManager
	outbounds []adapter.Outbound
}

func (m *mockOutboundManager) Outbounds() []adapter.Outbound {
	return m.outbounds
}

func (m *mockOutboundManager) Remove(tag string) error {
	m.outbounds = testRemove(m.outbounds, tag)
	return nil
}

func (m *mockOutboundManager) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag, outboundType string, options any) error {
	m.Remove(tag)
	m.outbounds = append(m.outbounds, &mockEndpoint{typ: outboundType, tag: tag})
	return nil
}

type mockEndpointManager struct {
	adapter.EndpointManager
	endpoints []adapter.Endpoint
}

func (m *mockEndpointManager) Endpoints() []adapter.Endpoint {
	return m.endpoints
}

func (m *mockEndpointManager) Remove(tag string) error {
	m.endpoints = testRemove(m.endpoints, tag)
	return nil
}

func (m *mockEndpointManager) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, typ string, options interface{}) error {
	m.Remove(tag)
	m.endpoints = append(m.endpoints, &mockEndpoint{typ: typ, tag: tag})
	return nil
}

func testRemove[T adapter.Outbound](list []T, tag string) []T {
	idx := slices.IndexFunc(list, func(e T) bool {
		return e.Tag() == tag
	})
	if idx == -1 {
		return list
	}
	return slices.Delete(list, idx, idx+1)
}

type mockEndpoint struct {
	adapter.Endpoint
	typ string
	tag string
}

func (m *mockEndpoint) Type() string { return m.typ }
func (m *mockEndpoint) Tag() string  { return m.tag }
