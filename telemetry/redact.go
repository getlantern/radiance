package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/getlantern/radiance/internal/redact"
)

// redactingExporter applies redact.Helper to every span it exports: the name,
// string attributes, events (including recorded errors), links and the status
// description. A helper tag is a client-side pseudonym for one of the user's
// pairings, and an error naming a helper outbound may carry the helper's
// address; clients must export neither. sing-box errors name the failing
// outbound, so any span that records one can carry both.
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
	return redact.Helper(s.ReadOnlySpan.Name())
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return redactAttrs(s.ReadOnlySpan.Attributes())
}

// Events returns the span's events, copied only if one needs redacting.
func (s redactedSpan) Events() []sdktrace.Event {
	events := s.ReadOnlySpan.Events()
	var out []sdktrace.Event
	for i, ev := range events {
		name := redact.Helper(ev.Name)
		attrs := redactAttrs(ev.Attributes)
		if out == nil && name == ev.Name && sameAttrs(attrs, ev.Attributes) {
			continue
		}
		if out == nil {
			out = append(make([]sdktrace.Event, 0, len(events)), events[:i]...)
		}
		ev.Name, ev.Attributes = name, attrs
		out = append(out, ev)
	}
	if out == nil {
		return events
	}
	return out
}

// Links returns the span's links, copied only if one needs redacting.
func (s redactedSpan) Links() []sdktrace.Link {
	links := s.ReadOnlySpan.Links()
	var out []sdktrace.Link
	for i, l := range links {
		attrs := redactAttrs(l.Attributes)
		if out == nil && sameAttrs(attrs, l.Attributes) {
			continue
		}
		if out == nil {
			out = append(make([]sdktrace.Link, 0, len(links)), links[:i]...)
		}
		l.Attributes = attrs
		out = append(out, l)
	}
	if out == nil {
		return links
	}
	return out
}

func (s redactedSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = redact.Helper(st.Description)
	return st
}

// sameAttrs reports whether redactAttrs returned its input unchanged.
func sameAttrs(a, b []attribute.KeyValue) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// redactAttrs returns attrs with string values redacted. It returns attrs
// itself when none needs it.
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
		if !strings.Contains(v, "helper") {
			return kv, false
		}
		r := redact.Helper(v)
		return attribute.String(string(kv.Key), r), r != v
	case attribute.STRINGSLICE:
		vs := kv.Value.AsStringSlice()
		changed := false
		for i, v := range vs {
			if r := redact.Helper(v); r != v {
				vs[i] = r
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
