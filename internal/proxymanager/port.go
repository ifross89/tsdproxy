// SPDX-FileCopyrightText: 2025 Paulo Almeida <almeidapaulopt@gmail.com>
// SPDX-License-Identifier: MIT

package proxymanager

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"

	"github.com/almeidapaulopt/tsdproxy/internal/consts"
	"github.com/almeidapaulopt/tsdproxy/internal/core"
	"github.com/almeidapaulopt/tsdproxy/internal/model"

	"github.com/rs/zerolog"
)

type port struct {
	log        zerolog.Logger
	ctx        context.Context
	listener   net.Listener
	cancel     context.CancelFunc
	handler    http.Handler
	httpServer *http.Server
	mtx        sync.Mutex
}

func newPortProxy(
	ctx context.Context,
	pconfig model.PortConfig,
	log zerolog.Logger,
	accessLog bool,
	whoisFunc func(next http.Handler) http.Handler,
) *port {
	//
	log = log.With().Str("port", pconfig.String()).Logger()

	ctxPort, cancel := context.WithCancel(ctx)

	// Create the reverse proxy
	//
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !pconfig.TLSValidate}, //nolint
	}
	reverseProxy := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(r *httputil.ProxyRequest) {
			targetURL := pconfig.GetFirstTarget()
			r.SetURL(targetURL)
			r.Out.Host = r.In.Host
			r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
			log.Debug().
				Str("method", r.In.Method).
				Str("host", r.In.Host).
				Str("path", r.In.URL.RequestURI()).
				Str("target", targetURL.String()).
				Msg("proxy rewrite")

			if user, ok := model.WhoisFromContext(r.In.Context()); ok {
				r.Out.Header.Set(consts.HeaderUsername, user.Username)
				r.Out.Header.Set(consts.HeaderDisplayName, user.DisplayName)
				r.Out.Header.Set(consts.HeaderProfilePicURL, user.ProfilePicURL)
			}

			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error().
				Err(err).
				Str("method", r.Method).
				Str("host", r.Host).
				Str("path", r.URL.RequestURI()).
				Str("target", pconfig.GetFirstTarget().String()).
				Msg("upstream proxy error")
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Debug().
				Int("status", resp.StatusCode).
				Str("method", resp.Request.Method).
				Str("host", resp.Request.Host).
				Str("path", resp.Request.URL.RequestURI()).
				Str("target", pconfig.GetFirstTarget().String()).
				Msg("upstream response")
			return nil
		},
	}

	handler := whoisFunc(reverseProxy)
	// add logger to proxy
	if accessLog {
		handler = core.LoggerMiddleware(log, handler)
	}

	// main http Server
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: core.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctxPort },
	}

	return &port{
		log:        log,
		ctx:        ctxPort,
		cancel:     cancel,
		handler:    handler,
		httpServer: httpServer,
	}
}

func newPortRedirect(ctx context.Context, pconfig model.PortConfig, log zerolog.Logger) *port {
	log = log.With().Str("port", pconfig.String()).Logger()

	ctxPort, cancel := context.WithCancel(ctx)

	redirectHTTPServer := &http.Server{
		ReadHeaderTimeout: core.ReadHeaderTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, pconfig.GetFirstTarget().String(), http.StatusMovedPermanently)
		}),
	}

	return &port{
		log:        log,
		ctx:        ctxPort,
		cancel:     cancel,
		handler:    redirectHTTPServer.Handler,
		httpServer: redirectHTTPServer,
	}
}

func (p *port) startWithListener(l net.Listener) error {
	p.mtx.Lock()
	p.listener = l
	p.mtx.Unlock()

	err := p.httpServer.Serve(l)
	defer p.log.Info().Msg("Terminating server")

	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("error starting port %w", err)
	}
	return nil
}

func (p *port) close() error {
	var errs error

	if p.httpServer != nil {
		errs = errors.Join(errs, p.httpServer.Shutdown(p.ctx))
	}

	if p.listener != nil {
		errs = errors.Join(errs, p.listener.Close())
	}

	p.cancel()

	return errs
}
