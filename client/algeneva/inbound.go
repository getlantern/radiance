package algeneva

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/getlantern/algeneva"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/uot"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/buf"
	singbufio "github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/pipe"
)

type AlgenevaInboundOptions struct {
	option.ListenOptions
	TLSConfig *tls.Config `json:"tlsConfig,omitempty"`
}

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[AlgenevaInboundOptions](registry, "algeneva", NewInbound)
}

type Inbound struct {
	inbound.Adapter
	router    adapter.ConnectionRouterEx
	logger    log.ContextLogger
	listener  *listener.Listener
	tlsConfig *tls.Config
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options AlgenevaInboundOptions) (adapter.Inbound, error) {
	fmt.Println("Creating algeneva inbound")
	inbound := &Inbound{
		Adapter:   inbound.NewAdapter("algeneva", tag),
		router:    uot.NewRouter(router, logger),
		logger:    logger,
		tlsConfig: options.TLSConfig,
	}
	inbound.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            logger,
		Network:           []string{"tcp"},
		Listen:            options.ListenOptions,
		ConnectionHandler: inbound,
	})
	return inbound, nil
}

func (ai *Inbound) Start(stage adapter.StartStage) error {
	ai.logger.Debug("Starting algeneva inbound")
	if stage != adapter.StartStateStart {
		return nil
	}
	return ai.listener.Start()
}

func (ai *Inbound) Close() error {
	return common.Close(ai.listener)
}

func (ai *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose network.CloseHandlerFunc) {
	wsc, err := ai.newConnection(ctx, conn)
	if err != nil {
		network.CloseOnHandshakeFailure(conn, onClose, err)
		ai.logger.ErrorContext(ctx, err, " algeneva handshake ", metadata.Source)
		return
	}
	conn = wsc
	ai.logger.DebugContext(ctx, "handling algeneva connection")
	err = HandleConnectionEx(ctx, conn, bufio.NewReader(conn), adapter.NewUpstreamHandlerEx(metadata, ai.newUserConnection, nil), metadata.Source, onClose)
	if err != nil {
		network.CloseOnHandshakeFailure(conn, onClose, err)
		ai.logger.ErrorContext(ctx, err, " process connection from ", metadata.Source)
	}
}

func (ai *Inbound) newConnection(ctx context.Context, conn net.Conn) (net.Conn, error) {
	err := ai.handshake(ctx, conn)
	if err != nil {
		ai.logger.ErrorContext(ctx, "algeneva handshake ", err)
		return nil, fmt.Errorf("algeneva read request: %w", err)
	}

	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		ai.logger.ErrorContext(ctx, "http read request: ", err)
		return nil, err
	}
	rw := newResWriter(conn)
	ai.logger.DebugContext(ctx, "websocket accept")
	wsc, err := websocket.Accept(rw, request, nil)
	if err != nil {
		return nil, fmt.Errorf("algeneva websocket accept: %w", err)
	}
	ai.logger.DebugContext(ctx, "websocket connection established")
	return websocket.NetConn(ctx, wsc, websocket.MessageBinary), nil
}

func (ai *Inbound) handshake(ctx context.Context, conn net.Conn) error {
	reader := bufio.NewReader(conn)
	ai.logger.TraceContext(ctx, "reading request")
	request, err := algeneva.ReadRequest(reader)
	if err != nil {
		return fmt.Errorf("algeneva read request: %w", err)
	}
	if request.Method != "CONNECT" {
		return fmt.Errorf("unexpected method: %s", request.Method)
	}
	ai.logger.TraceContext(ctx, "sending response")
	_, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	return err
}

// resWriter is a custom implementation of http.ResponseWriter and http.Hijacker
type resWriter struct {
	conn        net.Conn
	bufw        *bufio.Writer
	statusCode  int
	headers     http.Header
	wroteHeader bool
}

// newResWriter creates a new instance of resWriter
func newResWriter(conn net.Conn) *resWriter {
	return &resWriter{
		conn:       conn,
		bufw:       bufio.NewWriter(conn),
		statusCode: http.StatusOK, // Default status code
		headers:    make(http.Header),
	}
}

// Header returns the response headers
func (rw *resWriter) Header() http.Header {
	return rw.headers
}

// Write writes the response body
func (rw *resWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(rw.statusCode)
	}
	n, err := rw.bufw.Write(b)
	rw.bufw.Flush()
	return n, err
}

func (rw *resWriter) WriteHeader(statusCode int) {
	_, err := fmt.Fprintf(rw.conn, "HTTP/1.1 %d %s\r\n", statusCode, http.StatusText(statusCode))
	if err != nil {
		fmt.Println("error writing header: ", err)
		return
	}
	err = rw.Header().Write(rw.bufw)
	if err != nil {
		fmt.Println("error writing header: ", err)
		return
	}
	rw.bufw.WriteString("\r\n")
	rw.bufw.Flush()
	rw.wroteHeader = true
}

// Hijack allows the caller to take over the connection
func (rw *resWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return rw.conn,
		bufio.NewReadWriter(
			bufio.NewReader(rw.conn),
			rw.bufw,
		), nil
}

// copied from sing-box http protocol.
func HandleConnectionEx(
	ctx context.Context,
	conn net.Conn,
	reader *bufio.Reader,
	handler network.TCPConnectionHandlerEx,
	source metadata.Socksaddr,
	onClose network.CloseHandlerFunc,
) error {
	for {
		request, err := http.ReadRequest(reader)
		if err != nil {
			return fmt.Errorf("http: read request: %w", err)
		}
		request.Header.Set("Host", request.Host)

		if sourceAddress := SourceAddress(request); sourceAddress.IsValid() {
			source = sourceAddress
		}
		if request.Method == "CONNECT" {
			destination := metadata.ParseSocksaddrHostPortStr(request.URL.Hostname(), request.URL.Port())
			if destination.Port == 0 {
				switch request.URL.Scheme {
				case "https", "wss":
					destination.Port = 443
				default:
					destination.Port = 80
				}
			}
			_, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			if err != nil {
				return fmt.Errorf("http: write connect response: %w", err)
			}
			var requestConn net.Conn
			if reader.Buffered() > 0 {
				buffer := buf.NewSize(reader.Buffered())
				_, err = buffer.ReadFullFrom(reader, reader.Buffered())
				if err != nil {
					return err
				}
				fmt.Println("creating new cached conn")
				requestConn = singbufio.NewCachedConn(conn, buffer)
			} else {
				requestConn = conn
			}
			fmt.Println("handing off connection")
			handler.NewConnectionEx(ctx, requestConn, source, destination, onClose)
			return nil
		} else if strings.ToLower(request.Header.Get("Connection")) == "upgrade" {
			destination := metadata.ParseSocksaddrHostPortStr(request.URL.Hostname(), request.URL.Port())
			if destination.Port == 0 {
				switch request.URL.Scheme {
				case "https", "wss":
					destination.Port = 443
				default:
					destination.Port = 80
				}
			}
			serverConn, clientConn := pipe.Pipe()
			go func() {
				handler.NewConnectionEx(ctx, clientConn, source, destination, func(it error) {
					if it != nil {
						common.Close(serverConn, clientConn)
					}
				})
			}()
			err = request.Write(serverConn)
			if err != nil {
				return fmt.Errorf("http: write upgrade request: %w", err)
			}
			if reader.Buffered() > 0 {
				_, err = io.CopyN(serverConn, reader, int64(reader.Buffered()))
				if err != nil {
					return err
				}
			}
			return singbufio.CopyConn(ctx, conn, serverConn)
		} else {
			err = handleHTTPConnection(ctx, conn, handler, request, source)
			if err != nil {
				return err
			}
		}
	}
}

////////////////////////////////////////////////////////////////////////////////////////
//		Everything below this line is copied from sing-box http protocol
////////////////////////////////////////////////////////////////////////////////////////

func (h *Inbound) newUserConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose network.CloseHandlerFunc) {
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	h.logger.TraceContext(ctx, "inbound connection to ", metadata.Destination)
	h.router.RouteConnectionEx(ctx, conn, metadata, onClose)
}

func SourceAddress(request *http.Request) metadata.Socksaddr {
	address := metadata.ParseSocksaddr(request.RemoteAddr)
	forwardFrom := request.Header.Get("X-Forwarded-For")
	if forwardFrom != "" {
		for _, from := range strings.Split(forwardFrom, ",") {
			originAddr := metadata.ParseAddr(from)
			if originAddr.IsValid() {
				address.Addr = originAddr
				break
			}
		}
	}
	return address.Unwrap()
}

func handleHTTPConnection(
	ctx context.Context,
	conn net.Conn,
	handler network.TCPConnectionHandlerEx,
	request *http.Request, source metadata.Socksaddr,
) error {
	keepAlive := !(request.ProtoMajor == 1 && request.ProtoMinor == 0) && strings.TrimSpace(strings.ToLower(request.Header.Get("Proxy-Connection"))) == "keep-alive"
	request.RequestURI = ""

	removeHopByHopHeaders(request.Header)
	removeExtraHTTPHostPort(request)

	if hostStr := request.Header.Get("Host"); hostStr != "" {
		if hostStr != request.URL.Host {
			request.Host = hostStr
		}
	}

	if request.URL.Scheme == "" || request.URL.Host == "" {
		return responseWith(request, http.StatusBadRequest).Write(conn)
	}

	var innerErr atomic.TypedValue[error]
	httpClient := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				input, output := pipe.Pipe()
				go handler.NewConnectionEx(ctx, output, source, metadata.ParseSocksaddr(address), func(it error) {
					innerErr.Store(it)
					common.Close(input, output)
				})
				return input, nil
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer httpClient.CloseIdleConnections()

	requestCtx, cancel := context.WithCancel(ctx)
	response, err := httpClient.Do(request.WithContext(requestCtx))
	if err != nil {
		cancel()
		return errors.Join(innerErr.Load(), err, responseWith(request, http.StatusBadGateway).Write(conn))
	}

	removeHopByHopHeaders(response.Header)

	if keepAlive {
		response.Header.Set("Proxy-Connection", "keep-alive")
		response.Header.Set("Connection", "keep-alive")
		response.Header.Set("Keep-Alive", "timeout=4")
	}

	response.Close = !keepAlive

	err = response.Write(conn)
	if err != nil {
		cancel()
		return errors.Join(innerErr.Load(), err)
	}

	cancel()
	if !keepAlive {
		return conn.Close()
	}
	return nil
}

func removeHopByHopHeaders(header http.Header) {
	// Strip hop-by-hop header based on RFC:
	// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html#sec13.5.1
	// https://www.mnot.net/blog/2011/07/11/what_proxies_must_do

	header.Del("Proxy-Connection")
	header.Del("Proxy-Authenticate")
	header.Del("Proxy-Authorization")
	header.Del("TE")
	header.Del("Trailers")
	header.Del("Transfer-Encoding")
	header.Del("Upgrade")

	connections := header.Get("Connection")
	header.Del("Connection")
	if len(connections) == 0 {
		return
	}
	for _, h := range strings.Split(connections, ",") {
		header.Del(strings.TrimSpace(h))
	}
}

func removeExtraHTTPHostPort(req *http.Request) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	if pHost, port, err := net.SplitHostPort(host); err == nil && port == "80" {
		if metadata.ParseAddr(pHost).Is6() {
			pHost = "[" + pHost + "]"
		}
		host = pHost
	}

	req.Host = host
	req.URL.Host = host
}

func responseWith(request *http.Request, statusCode int, headers ...string) *http.Response {
	var header http.Header
	if len(headers) > 0 {
		header = make(http.Header)
		for i := 0; i < len(headers); i += 2 {
			header.Add(headers[i], headers[i+1])
		}
	}
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Proto:      request.Proto,
		ProtoMajor: request.ProtoMajor,
		ProtoMinor: request.ProtoMinor,
		Header:     header,
	}
}
