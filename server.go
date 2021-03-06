package air

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// server is an HTTP server.
type server struct {
	server *http.Server
}

// theServer is the singleton of the `server`.
var theServer = &server{
	server: &http.Server{},
}

// serve starts the s.
func (s *server) serve() error {
	s.server.Addr = Address
	s.server.Handler = s
	s.server.ReadTimeout = ReadTimeout
	s.server.ReadHeaderTimeout = ReadHeaderTimeout
	s.server.WriteTimeout = WriteTimeout
	s.server.IdleTimeout = IdleTimeout
	s.server.MaxHeaderBytes = MaxHeaderBytes
	s.server.ErrorLog = log.New(&serverErrorLogWriter{}, "air: ", 0)

	if DebugMode {
		LoggerLowestLevel = LoggerLevelDebug
		DEBUG("air: serving in debug mode")
	}

	if TLSCertFile != "" && TLSKeyFile != "" {
		host := s.server.Addr
		if strings.Contains(host, ":") {
			var err error
			if host, _, err = net.SplitHostPort(host); err != nil {
				return err
			}
		}

		var h2hs http.HandlerFunc
		h2hs = func(rw http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}

			http.Redirect(rw, r, "https://"+host+r.RequestURI, 301)
		}

		tlsCertFile, tlsKeyFile := TLSCertFile, TLSKeyFile
		if tlsCertFile == "Let's Encrypt" && tlsKeyFile == tlsCertFile {
			acm := autocert.Manager{
				Prompt: autocert.AcceptTOS,
				Cache:  autocert.DirCache(ACMECertRoot),
			}
			if len(HostWhitelist) > 0 {
				acm.HostPolicy = autocert.HostWhitelist(
					HostWhitelist...,
				)
			}

			go http.ListenAndServe(
				host+":http",
				acm.HTTPHandler(h2hs),
			)

			s.server.Addr = host + ":https"
			s.server.TLSConfig = acm.TLSConfig()
			tlsCertFile, tlsKeyFile = "", ""
		} else if HTTPSEnforced {
			go http.ListenAndServe(host+":http", h2hs)
		}

		return s.server.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	}

	return s.server.ListenAndServe()
}

// close closes the s immediately.
func (s *server) close() error {
	return s.server.Close()
}

// shutdown gracefully shuts down the s without interrupting any active
// connections until timeout. It waits indefinitely for connections to return to
// idle and then shut down when the timeout is less than or equal to zero.
func (s *server) shutdown(timeout time.Duration) error {
	if timeout <= 0 {
		return s.server.Shutdown(context.Background())
	}

	c, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return s.server.Shutdown(c)
}

// ServeHTTP implements the `http.Handler`.
func (s *server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// Check host

	if len(HostWhitelist) > 0 {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}

		allowed := false
		for _, h := range HostWhitelist {
			if h == host {
				allowed = true
				break
			}
		}

		if !allowed {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}

			http.Redirect(
				rw,
				r,
				scheme+"://"+HostWhitelist[0]+r.RequestURI,
				301,
			)

			return
		}
	}

	// Request

	req := &Request{
		Method:        r.Method,
		Scheme:        "http",
		Authority:     r.Host,
		Path:          r.RequestURI,
		Headers:       make(map[string]*Header, len(r.Header)),
		Body:          r.Body,
		ContentLength: r.ContentLength,
		Cookies:       map[string]*Cookie{},
		Params: make(
			map[string]*RequestParam,
			theRouter.maxParams,
		),
		RemoteAddress: r.RemoteAddr,
		ClientAddress: r.RemoteAddr,
		Values:        map[string]interface{}{},

		request:          r,
		parseCookiesOnce: &sync.Once{},
		parseParamsOnce:  &sync.Once{},
	}

	if r.TLS != nil {
		req.Scheme = "https"
	}

	for n, vs := range r.Header {
		h := &Header{
			Name:   strings.ToLower(n),
			Values: vs,
		}

		req.Headers[h.Name] = h
	}

	if f := req.Headers["forwarded"].FirstValue(); f != "" { // See RFC 7239
		for _, p := range strings.Split(strings.Split(f, ",")[0], ";") {
			p := strings.TrimSpace(p)
			if strings.HasPrefix(p, "for=") {
				req.ClientAddress = strings.TrimSuffix(
					strings.TrimPrefix(p[4:], "\"["),
					"]\"",
				)
				break
			}
		}
	} else if xff := req.Headers["x-forwarded-for"].FirstValue(); xff !=
		"" {
		req.ClientAddress = strings.TrimSpace(
			strings.Split(xff, ",")[0],
		)
	}

	theI18n.localize(req)

	// Response

	res := &Response{
		Status:  200,
		Headers: map[string]*Header{},
		Cookies: map[string]*Cookie{},

		request: req,
		writer:  rw,
	}

	// Chain gases

	h := func(req *Request, res *Response) error {
		rh := theRouter.route(req)
		h := func(req *Request, res *Response) error {
			if err := rh(req, res); err != nil {
				return err
			} else if !res.Written {
				return res.Write(nil)
			}

			return nil
		}

		req.ParseCookies()
		req.ParseParams()

		for i := len(Gases) - 1; i >= 0; i-- {
			h = Gases[i](h)
		}

		return h(req, res)
	}

	// Chain pregases

	for i := len(Pregases) - 1; i >= 0; i-- {
		h = Pregases[i](h)
	}

	// Execute chain

	if err := h(req, res); err != nil {
		ErrorHandler(err, req, res)
	}

	// Close opened request param file values

	for _, p := range req.Params {
		for _, pv := range p.Values {
			if pv.f != nil && pv.f.f != nil {
				pv.f.f.Close()
			}
		}
	}
}

// serverErrorLogWriter is an HTTP server error log writer.
type serverErrorLogWriter struct{}

// Write implements the `io.Writer`.
func (selw *serverErrorLogWriter) Write(b []byte) (int, error) {
	ERROR(string(b))
	return len(b), nil
}
