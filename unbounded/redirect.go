package unbounded

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxSignalingRedirects matches net/http's default redirect limit.
const maxSignalingRedirects = 10

// sameOriginRedirects returns a copy of c, or of a zero client if c is nil,
// that follows redirects only within the request's origin. Its transport and
// timeout are kept.
func sameOriginRedirects(c *http.Client) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	out.CheckRedirect = checkSignalingRedirect
	return out
}

// checkSignalingRedirect is the widget's signaling CheckRedirect, mirroring
// lantern-box's for the consumer. It follows a redirect only within the
// origin (scheme, host and port) of the request it came from. net/http would
// otherwise replay a 307 or 308'd genesis POST, whose form carries the
// widget's room tickets, to whatever another origin names, even over plain
// HTTP.
func checkSignalingRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxSignalingRedirects {
		return fmt.Errorf("unbounded: signaling: stopped after %d redirects", maxSignalingRedirects)
	}
	if len(via) > 0 && !sameOrigin(via[len(via)-1].URL, req.URL) {
		return errors.New("unbounded: signaling: refusing a redirect to another origin")
	}
	return nil
}

// sameOrigin reports whether a and b share scheme, host and port, with each
// scheme's default port made explicit.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	}
	return ""
}
