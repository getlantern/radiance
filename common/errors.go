package common

import "errors"

// ErrNotImplemented is returned by functions which have not yet been implemented. The existence of
// this error is temporary; this will go away when the API stabilized.
var ErrNotImplemented = errors.New("not yet implemented")
