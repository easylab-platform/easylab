package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/gorilla/websocket"

	"easyvcs-ext-ops/internal/worker"
)

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// wsProxy forwards a WebSocket connection to the session worker's /ws
// (RPC + job.completed broadcast).
func (s *server) wsProxy(w http.ResponseWriter, r *http.Request) {
	s.proxyWS(w, r, "")
}

// wsProxyJob proxies the session worker's per-job SSE stream (/ws/job) as
// plain HTTP — the worker now serves job output over Server-Sent Events, not
// WebSocket, so this is an ordinary streaming reverse proxy.
func (s *server) wsProxyJob(w http.ResponseWriter, r *http.Request) {
	session := param(r, "session")
	info, err := s.workerInfo(r.Context(), sessionKey(session))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if info.WorkerURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "worker not ready")
		return
	}

	target, err := url.Parse(info.WorkerURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = "/ws/job"
			// RawQuery (job_id=…) is preserved from the inbound request.
		},
		FlushInterval: -1, // stream each SSE event immediately
	}
	rp.ServeHTTP(w, r)
}

func (s *server) proxyWS(w http.ResponseWriter, r *http.Request, suffix string) {
	session := param(r, "session")
	info, err := s.workerInfo(r.Context(), sessionKey(session))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if info.WorkerURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "worker not ready")
		return
	}
	upstream := worker.ToWsURL(info.WorkerURL) + suffix
	if q := r.URL.RawQuery; q != "" {
		upstream += "?" + q
	}

	client, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()

	backend, _, err := websocket.DefaultDialer.Dial(upstream, nil)
	if err != nil {
		return
	}
	defer backend.Close()

	errCh := make(chan error, 2)
	go pumpWS(client, backend, errCh)
	go pumpWS(backend, client, errCh)
	<-errCh
}

func pumpWS(dst, src *websocket.Conn, errCh chan error) {
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		if err := dst.WriteMessage(mt, msg); err != nil {
			errCh <- err
			return
		}
	}
}
