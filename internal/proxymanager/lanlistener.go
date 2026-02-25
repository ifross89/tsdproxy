// SPDX-FileCopyrightText: 2025 Paulo Almeida <almeidapaulopt@gmail.com>
// SPDX-License-Identifier: MIT

package proxymanager

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/almeidapaulopt/tsdproxy/internal/core"

	"github.com/rs/zerolog"
)

type lanRoute struct {
	proxy   *Proxy
	handler http.Handler
}

type lanListener struct {
	log zerolog.Logger

	addr string

	server   *http.Server
	listener net.Listener

	routes map[string]lanRoute
	mtx    sync.RWMutex
}

func newLANListener(log zerolog.Logger, addr string) *lanListener {
	ll := &lanListener{
		log:    log.With().Str("module", "lanlistener").Logger(),
		addr:   addr,
		routes: make(map[string]lanRoute),
	}

	ll.server = &http.Server{
		Handler:           http.HandlerFunc(ll.serveHTTP),
		ReadHeaderTimeout: core.ReadHeaderTimeout,
	}

	return ll
}

func (l *lanListener) start() error {
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return err
	}

	tlsLn := tls.NewListener(ln, &tls.Config{ //nolint:gosec
		GetCertificate: l.getCertificate,
		MinVersion:     tls.VersionTLS12,
	})

	l.mtx.Lock()
	l.listener = tlsLn
	l.mtx.Unlock()

	go func() {
		if err := l.server.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			l.log.Error().Err(err).Msg("LAN listener stopped with error")
		}
	}()

	l.log.Info().Str("addr", l.addr).Msg("LANListener started")

	return nil
}

func (l *lanListener) close(ctx context.Context) error {
	var err error

	if l.server != nil {
		err = errors.Join(err, l.server.Shutdown(ctx))
	}

	l.mtx.RLock()
	ln := l.listener
	l.mtx.RUnlock()
	if ln != nil {
		err = errors.Join(err, ln.Close())
	}

	return err
}

func (l *lanListener) register(proxy *Proxy) error {
	handler, err := proxy.GetLANHandler()
	if err != nil {
		return err
	}

	host := normalizeLANHostname(proxy.Config.Hostname)
	if host == "" {
		return errors.New("invalid proxy hostname for LANListener")
	}

	l.mtx.Lock()
	l.routes[host] = lanRoute{proxy: proxy, handler: handler}
	l.mtx.Unlock()

	l.log.Info().Str("hostname", host).Msg("LANListener registered hostname")
	return nil
}

func (l *lanListener) unregister(hostname string) {
	host := normalizeLANHostname(hostname)
	if host == "" {
		return
	}

	l.mtx.Lock()
	delete(l.routes, host)
	l.mtx.Unlock()

	l.log.Info().Str("hostname", host).Msg("LANListener unregistered hostname")
}

func (l *lanListener) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := normalizeLANHostname(r.Host)
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	l.mtx.RLock()
	route, ok := l.routes[host]
	l.mtx.RUnlock()
	if !ok || route.handler == nil {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}

	route.handler.ServeHTTP(w, r)
}

func (l *lanListener) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := normalizeLANHostname(hello.ServerName)
	if host == "" {
		return nil, errors.New("missing SNI server name")
	}

	l.mtx.RLock()
	route, ok := l.routes[host]
	l.mtx.RUnlock()
	if !ok || route.proxy == nil {
		return nil, errors.New("unknown SNI host")
	}

	return route.proxy.GetTLSCertificate(host)
}

func normalizeLANHostname(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.Trim(host, "[]")
	}

	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")

	return host
}
