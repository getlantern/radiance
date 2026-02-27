package kindling

import (
	"io"
	"net/http"
)

const (
	ProxyPort = 14988
	ProxyAddr = "127.0.0.1:14988"
)

type KindlingProxy struct {
	server *http.Server
}

func NewKindlingProxy(addr string) *KindlingProxy {
	proxy := &KindlingProxy{
		server: &http.Server{
			Addr: addr,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Scheme == "" {
					r.URL.Scheme = "https"
				}
				if r.URL.Host == "" {
					r.URL.Host = r.Host
				}

				outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				outReq.Header = r.Header.Clone()

				resp, err := HTTPClient().Do(outReq)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()

				for key, values := range resp.Header {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
			}),
		},
	}
	return proxy
}

func (p *KindlingProxy) ListenAndServe() error {
	return p.server.ListenAndServe()
}

func (p *KindlingProxy) Close() error {
	return p.server.Close()
}

func (p *KindlingProxy) Addr() string {
	return p.server.Addr
}
