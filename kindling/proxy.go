package kindling

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
				if r.Method != http.MethodConnect {
					http.Error(w, "only CONNECT method is supported", http.StatusMethodNotAllowed)
					return
				}
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
					return
				}
				clientConn, _, err := hijacker.Hijack()
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				defer clientConn.Close()

				// Send 200 OK to client
				_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				if err != nil {
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(clientConn))
				if err != nil {
					writeErrorToConn(clientConn, http.StatusBadRequest, err.Error())
					return
				}
				req.URL = &url.URL{
					Scheme:   "http",
					Host:     r.Host,
					Path:     req.URL.Path,
					RawQuery: req.URL.RawQuery,
				}
				req.RequestURI = ""
				resp, err := HTTPClient().Do(req)
				if err != nil {
					writeErrorToConn(clientConn, http.StatusBadGateway, err.Error())
					return
				}
				defer resp.Body.Close()

				resp.Write(clientConn)
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

func writeErrorToConn(conn net.Conn, statusCode int, msg string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n\r\n%s", statusCode, http.StatusText(statusCode), msg)
}
