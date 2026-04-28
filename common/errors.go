package common

import "errors"

// ErrNotImplemented is returned by functions that have not yet been implemented.
// It is temporary and will be removed once the API stabilizes.
var ErrNotImplemented = errors.New("not yet implemented")
