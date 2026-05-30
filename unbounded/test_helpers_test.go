package unbounded

// countingWidget is a fakeWidget variant whose Stop runs a caller-
// supplied callback before returning. Used by TestStartDuringStop
// to decrement the live-widget counter under the test's own gate.
type countingWidget struct {
	onStop func()
}

func (w *countingWidget) Stop() {
	if w.onStop != nil {
		w.onStop()
	}
}
