package telemetry

import (
	"context"
	"strings"

	lbA "github.com/getlantern/lantern-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// redactingExporter replaces helper- outbound tags with "helper" in every span
// it exports: the name, string attributes, events (including recorded errors)
// and the status description. A helper tag is a client-side pseudonym for one
// of the user's pairings, and clients must not export it. sing-box errors name
// the failing outbound, so any span that records one can carry a tag.
type redactingExporter struct {
	sdktrace.SpanExporter
}

func newRedactingExporter(e sdktrace.SpanExporter) sdktrace.SpanExporter {
	return redactingExporter{SpanExporter: e}
}

func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redactedSpan{ReadOnlySpan: s}
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Name() string {
	return lbA.RedactHelperTags(s.ReadOnlySpan.Name())
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return redactAttrs(s.ReadOnlySpan.Attributes())
}

func (s redactedSpan) Events() []sdktrace.Event {
	events := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(events))
	for i, ev := range events {
		ev.Name = lbA.RedactHelperTags(ev.Name)
		ev.Attributes = redactAttrs(ev.Attributes)
		out[i] = ev
	}
	return out
}

func (s redactedSpan) Links() []sdktrace.Link {
	links := s.ReadOnlySpan.Links()
	out := make([]sdktrace.Link, len(links))
	for i, l := range links {
		l.Attributes = redactAttrs(l.Attributes)
		out[i] = l
	}
	return out
}

func (s redactedSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = lbA.RedactHelperTags(st.Description)
	return st
}

// redactAttrs returns attrs with helper tags in string values redacted. It
// returns attrs itself when none needs it.
func redactAttrs(attrs []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for i, kv := range attrs {
		red, changed := redactValue(kv)
		if !changed {
			if out != nil {
				out = append(out, kv)
			}
			continue
		}
		if out == nil {
			out = append(make([]attribute.KeyValue, 0, len(attrs)), attrs[:i]...)
		}
		out = append(out, red)
	}
	if out == nil {
		return attrs
	}
	return out
}

func redactValue(kv attribute.KeyValue) (attribute.KeyValue, bool) {
	switch kv.Value.Type() {
	case attribute.STRING:
		v := kv.Value.AsString()
		if !strings.Contains(v, lbA.HelperTagPrefix) {
			return kv, false
		}
		return attribute.String(string(kv.Key), lbA.RedactHelperTags(v)), true
	case attribute.STRINGSLICE:
		vs := kv.Value.AsStringSlice()
		changed := false
		for i, v := range vs {
			if strings.Contains(v, lbA.HelperTagPrefix) {
				vs[i] = lbA.RedactHelperTags(v)
				changed = true
			}
		}
		if !changed {
			return kv, false
		}
		return attribute.StringSlice(string(kv.Key), vs), true
	}
	return kv, false
}
