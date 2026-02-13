package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultHostnameSuffix = "dv.localhost"
const defaultHappyProxyCacheMaxEntries = 512

var (
	errAutoHealDisabled     = errors.New("auto-heal disabled")
	errAutoHealUnavailable  = errors.New("auto-heal unavailable")
	errContainerNotFound    = errors.New("container not found")
	errContainerNotRunning  = errors.New("container not running")
	errContainerNoIP        = errors.New("container has no IP")
	errHostContainerInvalid = errors.New("host does not map to a container")

	// Match Docker-compatible container names: [a-z0-9][a-z0-9_.-]*
	dockerContainerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

type route struct {
	Host   string `json:"host"`
	Target string `json:"target"`
}

type proxyTable struct {
	mu     sync.RWMutex
	routes map[string]*url.URL
}

func newProxyTable() *proxyTable {
	return &proxyTable{
		routes: map[string]*url.URL{},
	}
}

func (p *proxyTable) set(host string, target *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[host] = target
}

func (p *proxyTable) delete(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.routes[host]; !ok {
		return false
	}
	delete(p.routes, host)
	return true
}

func (p *proxyTable) list() []route {
	p.mu.RLock()
	defer p.mu.RUnlock()
	hosts := make([]string, 0, len(p.routes))
	for h := range p.routes {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	out := make([]route, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, route{
			Host:   h,
			Target: p.routes[h].String(),
		})
	}
	return out
}

func (p *proxyTable) lookup(host string) *url.URL {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.routes[host]
}

type healCall struct {
	target *url.URL
	err    error
	done   chan struct{}
}

type containerInspect struct {
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type containerInspector interface {
	InspectContainer(ctx context.Context, containerName string) (*containerInspect, error)
}

type dockerInspector struct {
	http    *http.Client
	baseURL string
}

func newDockerInspector(socketPath string, timeout time.Duration) containerInspector {
	if strings.TrimSpace(socketPath) == "" {
		return nil
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &dockerInspector{
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		baseURL: "http://unix",
	}
}

func (d *dockerInspector) InspectContainer(ctx context.Context, containerName string) (*containerInspect, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/containers/"+url.PathEscape(containerName)+"/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAutoHealUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: docker inspect failed: %s", errAutoHealUnavailable, strings.TrimSpace(readErrorBody(resp.Body)))
	}
	var info containerInspect
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("%w: invalid docker inspect response", errAutoHealUnavailable)
	}
	return &info, nil
}

type routeHealer struct {
	table         *proxyTable
	inspector     containerInspector
	hostSuffix    string
	containerPort int
	autoHeal      bool
	timeout       time.Duration

	mu       sync.Mutex
	inflight map[string]*healCall
}

func newRouteHealer(table *proxyTable, inspector containerInspector, hostSuffix string, containerPort int, autoHeal bool, timeout time.Duration) *routeHealer {
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	if containerPort <= 0 {
		containerPort = 4200
	}
	if strings.TrimSpace(hostSuffix) == "" {
		hostSuffix = defaultHostnameSuffix
	}
	return &routeHealer{
		table:         table,
		inspector:     inspector,
		hostSuffix:    hostSuffix,
		containerPort: containerPort,
		autoHeal:      autoHeal,
		timeout:       timeout,
		inflight:      make(map[string]*healCall),
	}
}

func (h *routeHealer) Heal(ctx context.Context, host string) (target *url.URL, err error) {
	if !h.autoHeal {
		return nil, errAutoHealDisabled
	}
	if h.inspector == nil {
		return nil, fmt.Errorf("%w: docker inspector unavailable", errAutoHealUnavailable)
	}

	h.mu.Lock()
	if call, ok := h.inflight[host]; ok {
		h.mu.Unlock()
		<-call.done
		return call.target, call.err
	}
	call := &healCall{done: make(chan struct{})}
	h.inflight[host] = call
	h.mu.Unlock()

	// Default to a non-nil error so coalesced waiters never observe (nil, nil)
	// if healOnce panics before assigning the actual result.
	err = fmt.Errorf("%w: heal did not complete", errAutoHealUnavailable)
	defer func() {
		call.target = target
		call.err = err
		close(call.done)

		h.mu.Lock()
		delete(h.inflight, host)
		h.mu.Unlock()
	}()

	// Coalesced healing should not be canceled by whichever caller won the
	// race. Use a detached base context and rely on the explicit heal timeout.
	target, err = h.healOnce(withoutCancel(ctx), host)

	return target, err
}

func withoutCancel(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (h *routeHealer) healOnce(ctx context.Context, host string) (*url.URL, error) {
	containerName, ok := containerNameFromHost(host, h.hostSuffix)
	if !ok {
		return nil, errHostContainerInvalid
	}

	healCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	inspect, err := h.inspector.InspectContainer(healCtx, containerName)
	if err != nil {
		return nil, err
	}
	if inspect == nil {
		return nil, fmt.Errorf("%w: empty inspect payload", errAutoHealUnavailable)
	}
	if !inspect.State.Running {
		status := strings.TrimSpace(inspect.State.Status)
		if status != "" {
			return nil, fmt.Errorf("%w: status=%s", errContainerNotRunning, status)
		}
		return nil, errContainerNotRunning
	}

	containerIP := firstContainerIP(inspect.NetworkSettings.Networks)
	if containerIP == "" {
		return nil, errContainerNoIP
	}

	target, err := parseTarget(fmt.Sprintf("http://%s:%d", containerIP, h.containerPort))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAutoHealUnavailable, err)
	}
	h.table.set(host, target)
	log.Printf("auto-healed route %s -> %s", host, target)
	return target, nil
}

func firstContainerIP(networks map[string]struct {
	IPAddress string `json:"IPAddress"`
}) string {
	if len(networks) == 0 {
		return ""
	}
	networkNames := make([]string, 0, len(networks))
	for name := range networks {
		networkNames = append(networkNames, name)
	}
	sort.Strings(networkNames)
	for _, name := range networkNames {
		if ip := strings.TrimSpace(networks[name].IPAddress); ip != "" {
			return ip
		}
	}
	return ""
}

type proxyServer struct {
	table            *proxyTable
	healer           *routeHealer
	diagnostics      bool
	diagnosticSuffix string

	happyProxyMu         sync.RWMutex
	happyProxy           map[string]*cachedHappyProxy
	happyProxyTick       atomic.Uint64
	happyProxyMaxEntries int
}

type diagnosticKind int

const (
	diagnosticKindNoRoute diagnosticKind = iota
	diagnosticKindUpstream
)

type cachedHappyProxy struct {
	target string
	proxy  *httputil.ReverseProxy
	usedAt atomic.Uint64
}

type proxyAttemptState struct {
	host    string
	retried atomic.Bool
}

type proxyAttemptStateKey struct{}

func newProxyServer(table *proxyTable, healer *routeHealer, diagnostics bool, hostSuffix string) *proxyServer {
	if strings.TrimSpace(hostSuffix) == "" {
		hostSuffix = defaultHostnameSuffix
	}
	return &proxyServer{
		table:                table,
		healer:               healer,
		diagnostics:          diagnostics,
		diagnosticSuffix:     hostSuffix,
		happyProxy:           make(map[string]*cachedHappyProxy),
		happyProxyMaxEntries: defaultHappyProxyCacheMaxEntries,
	}
}

func (s *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := normalizeHost(r.Host)
	if host == "" {
		http.Error(w, "missing host", http.StatusBadGateway)
		return
	}

	target := s.table.lookup(host)
	if target == nil {
		s.dropHappyPathProxy(host)
		healedTarget, err := s.healer.Heal(r.Context(), host)
		if err != nil {
			s.writeDiagnostic(w, r, host, diagnosticKindNoRoute, "", err)
			return
		}
		target = healedTarget
	}

	s.serveWithTarget(w, r, host, target)
}

func (s *proxyServer) serveWithTarget(w http.ResponseWriter, r *http.Request, host string, target *url.URL) {
	if target == nil {
		s.writeDiagnostic(w, r, host, diagnosticKindNoRoute, "", errHostContainerInvalid)
		return
	}

	state := &proxyAttemptState{host: host}
	r = r.WithContext(context.WithValue(r.Context(), proxyAttemptStateKey{}, state))
	s.happyPathProxy(host, target).ServeHTTP(w, r)
}

func (s *proxyServer) happyPathProxy(host string, target *url.URL) *httputil.ReverseProxy {
	targetStr := target.String()
	tick := s.happyProxyTick.Add(1)

	s.happyProxyMu.RLock()
	cached, ok := s.happyProxy[host]
	s.happyProxyMu.RUnlock()
	if ok && cached != nil && cached.target == targetStr && cached.proxy != nil {
		cached.usedAt.Store(tick)
		return cached.proxy
	}

	s.happyProxyMu.Lock()
	defer s.happyProxyMu.Unlock()
	cached, ok = s.happyProxy[host]
	if ok && cached != nil && cached.target == targetStr && cached.proxy != nil {
		cached.usedAt.Store(tick)
		return cached.proxy
	}

	if (!ok || cached == nil) && s.happyProxyMaxEntries > 0 && len(s.happyProxy) >= s.happyProxyMaxEntries {
		s.evictLeastRecentlyUsedHappyProxyLocked()
	}

	proxy := buildReverseProxy(host, target, s.handleHappyPathProxyError)
	entry := &cachedHappyProxy{
		target: targetStr,
		proxy:  proxy,
	}
	entry.usedAt.Store(tick)
	s.happyProxy[host] = entry
	return proxy
}

func (s *proxyServer) dropHappyPathProxy(host string) {
	s.happyProxyMu.Lock()
	delete(s.happyProxy, host)
	s.happyProxyMu.Unlock()
}

func (s *proxyServer) evictLeastRecentlyUsedHappyProxyLocked() {
	var (
		evictHost string
		evictTick uint64
		found     bool
	)
	for host, entry := range s.happyProxy {
		if entry == nil {
			evictHost = host
			found = true
			break
		}
		tick := entry.usedAt.Load()
		if !found || tick < evictTick {
			evictHost = host
			evictTick = tick
			found = true
		}
	}
	if found {
		delete(s.happyProxy, evictHost)
	}
}

func (s *proxyServer) handleHappyPathProxyError(w http.ResponseWriter, req *http.Request, proxyErr error) {
	host := normalizeHost(req.Host)
	state, _ := req.Context().Value(proxyAttemptStateKey{}).(*proxyAttemptState)
	if state != nil && strings.TrimSpace(state.host) != "" {
		host = state.host
	}

	if state != nil && s.healer != nil && isRetryableMethod(req.Method) && state.retried.CompareAndSwap(false, true) {
		healedTarget, healErr := s.healer.Heal(req.Context(), host)
		if healErr == nil && healedTarget != nil {
			retry := buildReverseProxy(host, healedTarget, func(w http.ResponseWriter, req *http.Request, retryErr error) {
				s.writeDiagnostic(w, req, host, diagnosticKindUpstream, classifyUpstreamError(retryErr), retryErr)
			})
			retry.ServeHTTP(w, req)
			return
		}
		if healErr != nil {
			s.writeDiagnostic(w, req, host, diagnosticKindUpstream, classifyUpstreamError(proxyErr), fmt.Errorf("%v (auto-heal failed: %w)", proxyErr, healErr))
			return
		}
	}

	s.writeDiagnostic(w, req, host, diagnosticKindUpstream, classifyUpstreamError(proxyErr), proxyErr)
}

func isRetryableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func classifyUpstreamError(err error) string {
	msg := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", err)))
	switch {
	case strings.Contains(msg, "connection refused"):
		return "Upstream connection refused"
	case strings.Contains(msg, "no route to host"):
		return "Upstream route unavailable"
	case strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context deadline exceeded"):
		return "Upstream timeout"
	default:
		return "Upstream unavailable"
	}
}

func classifyHealFailure(err error) string {
	switch {
	case errors.Is(err, errAutoHealDisabled):
		return "Auto-heal disabled"
	case errors.Is(err, errAutoHealUnavailable):
		return "Auto-heal unavailable"
	case errors.Is(err, errContainerNotFound):
		return "Container not found"
	case errors.Is(err, errContainerNotRunning):
		return "Container is not running"
	case errors.Is(err, errContainerNoIP):
		return "Container has no IP address"
	case errors.Is(err, errHostContainerInvalid):
		return "Host does not map to a known container"
	default:
		return "Auto-heal failed"
	}
}

var diagnosticIDCounter atomic.Uint64

func nextDiagnosticID() string {
	ts := uint64(time.Now().UnixNano())
	seq := diagnosticIDCounter.Add(1)
	return fmt.Sprintf("%x-%x", ts, seq)
}

func (s *proxyServer) writeDiagnostic(w http.ResponseWriter, r *http.Request, host string, kind diagnosticKind, category string, err error) {
	if !s.diagnostics {
		http.Error(w, "proxy request failed", http.StatusBadGateway)
		return
	}

	if kind == diagnosticKindNoRoute {
		category = classifyHealFailure(err)
	}
	containerName, _ := containerNameFromHost(host, s.diagnosticSuffix)
	diagnosticID := nextDiagnosticID()
	suggestions := []string{"dv list", "curl -sS http://127.0.0.1:2080/api/routes"}
	if containerName != "" {
		suggestions = append(suggestions, fmt.Sprintf("dv start %q", containerName))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = diagnosticTemplate.Execute(w, diagnosticView{
		Host:         host,
		Category:     category,
		Error:        strings.TrimSpace(fmt.Sprintf("%v", err)),
		DiagnosticID: diagnosticID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Suggestions:  suggestions,
	})
}

type diagnosticView struct {
	Host         string
	Category     string
	Error        string
	DiagnosticID string
	Timestamp    string
	Suggestions  []string
}

var diagnosticTemplate = template.Must(template.New("diagnostic").Parse(`<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>dv local proxy diagnostic</title>
    <style>
      :root {
        --bg: #f6f4ef;
        --panel: #ffffff;
        --ink: #1d1f24;
        --muted: #5d6470;
        --accent: #0f766e;
        --danger: #b42318;
        --border: #d8d3c6;
      }
      * { box-sizing: border-box; }
      body {
        margin: 0;
        font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, sans-serif;
        color: var(--ink);
        background:
          radial-gradient(circle at 90% 10%, #d6efe9 0%, transparent 40%),
          radial-gradient(circle at 0% 100%, #f6dbb9 0%, transparent 35%),
          var(--bg);
      }
      .wrap {
        max-width: 860px;
        margin: 32px auto;
        padding: 0 18px;
      }
      .panel {
        background: var(--panel);
        border: 1px solid var(--border);
        border-radius: 14px;
        padding: 22px;
        box-shadow: 0 8px 24px rgba(0,0,0,0.08);
      }
      h1 {
        margin: 0 0 10px;
        font-size: 1.45rem;
        line-height: 1.2;
      }
      .meta {
        color: var(--muted);
        margin: 4px 0 18px;
      }
      .pill {
        display: inline-block;
        background: #e7f4f2;
        color: var(--accent);
        border: 1px solid #b0ddd8;
        padding: 4px 10px;
        border-radius: 999px;
        margin-bottom: 14px;
        font-weight: 600;
      }
      .error {
        color: var(--danger);
        font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
        font-size: 0.92rem;
        background: #fff5f4;
        border: 1px solid #f2c8c5;
        border-radius: 10px;
        padding: 10px;
      }
      code {
        font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
        background: #f2f5f8;
        border: 1px solid #dde3ea;
        border-radius: 6px;
        padding: 2px 6px;
      }
      ul {
        margin: 10px 0 0;
        padding-left: 20px;
      }
      li { margin: 6px 0; }
    </style>
  </head>
  <body>
    <main class="wrap">
      <section class="panel">
        <h1>Proxy could not complete this request</h1>
        <p class="pill">{{.Category}}</p>
        <p class="meta">Host: <code>{{.Host}}</code></p>
        <p class="meta">Diagnostic ID: <code>{{.DiagnosticID}}</code> · Time: <code>{{.Timestamp}}</code></p>
        {{if .Error}}
          <p class="error">{{.Error}}</p>
        {{end}}
        <h2>Suggested checks</h2>
        <ul>
          {{range .Suggestions}}
            <li><code>{{.}}</code></li>
          {{end}}
        </ul>
      </section>
    </main>
  </body>
</html>`))

func containerNameFromHost(host, suffix string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	normalizedSuffix := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(suffix, ".")))
	if normalized == "" || normalizedSuffix == "" {
		return "", false
	}
	marker := "." + normalizedSuffix
	if !strings.HasSuffix(normalized, marker) {
		return "", false
	}
	base := strings.TrimSuffix(normalized, marker)
	if base == "" {
		return "", false
	}
	if !dockerContainerNamePattern.MatchString(base) {
		return "", false
	}
	return base, true
}

var hostnameSuffix string

func main() {
	httpAddr := envOrDefault("PROXY_HTTP_ADDR", ":80")
	httpsAddr := envOrDefault("PROXY_HTTPS_ADDR", "")
	apiAddr := envOrDefault("PROXY_API_ADDR", ":2080")
	tlsCertFile := envOrDefault("PROXY_TLS_CERT_FILE", "")
	tlsKeyFile := envOrDefault("PROXY_TLS_KEY_FILE", "")
	redirectHTTP := isTruthyEnv("PROXY_REDIRECT_HTTP_TO_HTTPS")
	externalHTTPSPort := envIntOrDefault("PROXY_EXTERNAL_HTTPS_PORT", 443)
	hostnameSuffix = envOrDefault("PROXY_HOSTNAME_SUFFIX", defaultHostnameSuffix)
	autoHeal := envBoolOrDefault("PROXY_AUTO_HEAL", true)
	diagnosticHTML := envBoolOrDefault("PROXY_DIAGNOSTIC_HTML", true)
	autoHealTimeout := time.Duration(envIntOrDefault("PROXY_AUTO_HEAL_TIMEOUT_MS", 1500)) * time.Millisecond
	autoHealContainerPort := envIntOrDefault("PROXY_AUTO_HEAL_CONTAINER_PORT", 4200)
	dockerSocketPath := envOrDefault("PROXY_DOCKER_SOCKET", "/var/run/docker.sock")

	table := newProxyTable()
	healer := newRouteHealer(table, newDockerInspector(dockerSocketPath, autoHealTimeout), hostnameSuffix, autoHealContainerPort, autoHeal, autoHealTimeout)
	proxyHandler := newProxyServer(table, healer, diagnosticHTML, hostnameSuffix)

	go func() {
		log.Printf("local-proxy admin listening on %s", apiAddr)
		admin := &http.Server{
			Addr:              apiAddr,
			Handler:           apiRouter(table, proxyHandler),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		if err := admin.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("admin server error: %v", err)
		}
	}()

	httpsEnabled := httpsAddr != "" || tlsCertFile != "" || tlsKeyFile != ""
	if redirectHTTP && !httpsEnabled {
		log.Fatalf("PROXY_REDIRECT_HTTP_TO_HTTPS requires PROXY_HTTPS_ADDR and TLS cert/key env vars")
	}
	if httpsEnabled {
		if httpsAddr == "" {
			httpsAddr = ":443"
		}
		if tlsCertFile == "" || tlsKeyFile == "" {
			log.Fatalf("PROXY_TLS_CERT_FILE and PROXY_TLS_KEY_FILE are required when PROXY_HTTPS_ADDR is set")
		}
		go func() {
			log.Printf("local-proxy HTTPS listening on %s", httpsAddr)
			server := &http.Server{
				Addr:              httpsAddr,
				Handler:           proxyHandler,
				ReadHeaderTimeout: 5 * time.Second,
				TLSConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			}
			if err := server.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("https server error: %v", err)
			}
		}()
	}

	var handler http.Handler = proxyHandler
	if httpsEnabled && redirectHTTP {
		handler = redirectToHTTPSHandler(externalHTTPSPort)
		log.Printf("local-proxy HTTP redirect listening on %s", httpAddr)
	} else {
		log.Printf("local-proxy HTTP listening on %s", httpAddr)
	}

	server := &http.Server{
		Addr:              httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("proxy server error: %v", err)
	}
}

func apiRouter(table *proxyTable, proxy *proxyServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/api/routes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(table.list()); err != nil {
				http.Error(w, "failed to encode routes", http.StatusInternalServerError)
			}
		case http.MethodPost:
			var payload route
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			host := normalizeHost(payload.Host)
			if host == "" {
				http.Error(w, fmt.Sprintf("host must end with .%s", hostnameSuffix), http.StatusBadRequest)
				return
			}
			target, err := parseTarget(payload.Target)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			table.set(host, target)
			log.Printf("registered route %s -> %s", host, target)
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/routes/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host := normalizeHost(strings.TrimPrefix(r.URL.Path, "/api/routes/"))
		if host == "" {
			http.Error(w, fmt.Sprintf("host must end with .%s", hostnameSuffix), http.StatusBadRequest)
			return
		}
		if !table.delete(host) {
			http.NotFound(w, r)
			return
		}
		if proxy != nil {
			proxy.dropHappyPathProxy(host)
		}
		log.Printf("removed route %s", host)
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func envOrDefault(key string, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func isTruthyEnv(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envBoolOrDefault(key string, fallback bool) bool {
	if _, ok := os.LookupEnv(key); !ok {
		return fallback
	}
	// Explicit env values always win: truthy values map to true and any other
	// set value maps to false. The fallback only applies when unset.
	return isTruthyEnv(key)
}

func envIntOrDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func redirectToHTTPSHandler(externalPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)
		if host == "" {
			http.Error(w, "missing host", http.StatusBadGateway)
			return
		}
		targetHost := host
		if externalPort > 0 && externalPort != 443 {
			targetHost = fmt.Sprintf("%s:%d", host, externalPort)
		}
		target := "https://" + targetHost + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return ""
	}
	if strings.ContainsAny(h, "/\\@") {
		return ""
	}
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		parsed, err := url.Parse(h)
		if err != nil {
			return ""
		}
		h = parsed.Host
	}
	if strings.Contains(h, ":") {
		hostOnly, _, err := net.SplitHostPort(h)
		if err == nil {
			h = hostOnly
		}
	}
	suffix := hostnameSuffix
	if suffix == "" {
		suffix = defaultHostnameSuffix
	}
	if !strings.HasSuffix(h, "."+suffix) {
		return ""
	}
	return h
}

func parseTarget(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("target required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid target: %v", err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("only http targets are supported right now")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("target host is required")
	}
	return u, nil
}

func buildReverseProxy(host string, target *url.URL, onError func(http.ResponseWriter, *http.Request, error)) *httputil.ReverseProxy {
	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		_, port := splitHostPortMaybe(req.Host)

		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		hostHeader := host
		if port != "" {
			hostHeader = host + ":" + port
		}
		req.Host = hostHeader
		req.Header.Set("X-Forwarded-Host", hostHeader)

		forwardedProto := "http"
		defaultPort := "80"
		if req.TLS != nil {
			forwardedProto = "https"
			defaultPort = "443"
		}
		if port != "" {
			defaultPort = port
		}
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
		req.Header.Set("X-Forwarded-Port", defaultPort)
		if forwardedProto == "https" {
			req.Header.Set("X-Forwarded-Ssl", "on")
		} else {
			req.Header.Del("X-Forwarded-Ssl")
		}

		if ip, _, err := net.SplitHostPort(req.RemoteAddr); err == nil && ip != "" {
			appendForwardedFor(req, ip)
		}
	}
	proxy := &httputil.ReverseProxy{
		Director:      director,
		FlushInterval: 50 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if onError != nil {
				onError(w, r, err)
				return
			}
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	return proxy
}

func splitHostPortMaybe(host string) (string, string) {
	if strings.Contains(host, ":") {
		hostOnly, port, err := net.SplitHostPort(host)
		if err == nil {
			return hostOnly, port
		}
	}
	return host, ""
}

func appendForwardedFor(req *http.Request, ip string) {
	const header = "X-Forwarded-For"
	if prior := req.Header.Get(header); prior != "" {
		req.Header.Set(header, prior+", "+ip)
	} else {
		req.Header.Set(header, ip)
	}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func readErrorBody(r io.Reader) string {
	if r == nil {
		return "no response body"
	}
	body, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return err.Error()
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return "empty response"
	}
	return msg
}
