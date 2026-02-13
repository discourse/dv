package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeInspector struct {
	mu    sync.Mutex
	calls int
	last  string
	info  *containerInspect
	err   error
	delay time.Duration
}

type panicInspector struct {
	entered chan struct{}
}

func (p *panicInspector) InspectContainer(_ context.Context, _ string) (*containerInspect, error) {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	panic("boom")
}

type contextAwareInspector struct {
	entered chan struct{}
	release chan struct{}
	info    *containerInspect
}

func (c *contextAwareInspector) InspectContainer(ctx context.Context, _ string) (*containerInspect, error) {
	select {
	case <-c.entered:
	default:
		close(c.entered)
	}

	select {
	case <-c.release:
		return c.info, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeInspector) InspectContainer(_ context.Context, containerName string) (*containerInspect, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = containerName
	return f.info, f.err
}

func (f *fakeInspector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestContainerNameFromHost(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		suffix   string
		want     string
		wantOkay bool
	}{
		{name: "valid", host: "api-key.home.arpa", suffix: "home.arpa", want: "api-key", wantOkay: true},
		{name: "trailing dot", host: "api-key.home.arpa.", suffix: "home.arpa", want: "api-key", wantOkay: true},
		{name: "valid dot and underscore", host: "api_key.v2.home.arpa", suffix: "home.arpa", want: "api_key.v2", wantOkay: true},
		{name: "invalid suffix", host: "api-key.dv.localhost", suffix: "home.arpa", wantOkay: false},
		{name: "empty base", host: "home.arpa", suffix: "home.arpa", wantOkay: false},
		{name: "leading punctuation not allowed", host: "..evil.home.arpa", suffix: "home.arpa", wantOkay: false},
		{name: "slash not allowed", host: "api/key.home.arpa", suffix: "home.arpa", wantOkay: false},
		{name: "space not allowed", host: "api key.home.arpa", suffix: "home.arpa", wantOkay: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := containerNameFromHost(tc.host, tc.suffix)
			if ok != tc.wantOkay {
				t.Fatalf("expected ok=%v, got %v (value=%q)", tc.wantOkay, ok, got)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestFirstContainerIPPrefersSortedNetwork(t *testing.T) {
	networks := map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{
		"zz": {IPAddress: "172.17.0.8"},
		"aa": {IPAddress: "172.17.0.7"},
	}
	if got := firstContainerIP(networks); got != "172.17.0.7" {
		t.Fatalf("expected 172.17.0.7, got %q", got)
	}
}

func TestRouteHealerHealSuccess(t *testing.T) {
	info := &containerInspect{}
	info.State.Running = true
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{
		"bridge": {IPAddress: "172.17.0.8"},
	}

	inspector := &fakeInspector{info: info}
	table := newProxyTable()
	healer := newRouteHealer(table, inspector, "home.arpa", 4200, true, time.Second)

	target, err := healer.Heal(context.Background(), "api-key.home.arpa")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if target == nil || target.String() != "http://172.17.0.8:4200" {
		t.Fatalf("unexpected target: %v", target)
	}
	if inspector.last != "api-key" {
		t.Fatalf("expected inspect for api-key, got %q", inspector.last)
	}
	if route := table.lookup("api-key.home.arpa"); route == nil || route.String() != "http://172.17.0.8:4200" {
		t.Fatalf("expected route in table, got %v", route)
	}
}

func TestRouteHealerContainerNotRunning(t *testing.T) {
	info := &containerInspect{}
	info.State.Running = false
	info.State.Status = "exited"

	healer := newRouteHealer(newProxyTable(), &fakeInspector{info: info}, "home.arpa", 4200, true, time.Second)
	_, err := healer.Heal(context.Background(), "api-key.home.arpa")
	if !errors.Is(err, errContainerNotRunning) {
		t.Fatalf("expected errContainerNotRunning, got %v", err)
	}
}

func TestRouteHealerSingleflightByHost(t *testing.T) {
	info := &containerInspect{}
	info.State.Running = true
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{
		"bridge": {IPAddress: "172.17.0.8"},
	}
	inspector := &fakeInspector{info: info, delay: 50 * time.Millisecond}
	healer := newRouteHealer(newProxyTable(), inspector, "home.arpa", 4200, true, time.Second)

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := healer.Heal(context.Background(), "api-key.home.arpa")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	}
	if inspector.callCount() != 1 {
		t.Fatalf("expected one inspect call, got %d", inspector.callCount())
	}
}

func TestProxyServerHappyPathProxyCache(t *testing.T) {
	table := newProxyTable()
	server := newProxyServer(table, nil, true, "home.arpa")

	targetA, err := parseTarget("http://127.0.0.1:4200")
	if err != nil {
		t.Fatalf("parse targetA: %v", err)
	}
	targetB, err := parseTarget("http://127.0.0.1:4201")
	if err != nil {
		t.Fatalf("parse targetB: %v", err)
	}

	p1 := server.happyPathProxy("api-key.home.arpa", targetA)
	p2 := server.happyPathProxy("api-key.home.arpa", targetA)
	if p1 != p2 {
		t.Fatal("expected cached happy-path proxy to be reused for same host+target")
	}

	p3 := server.happyPathProxy("api-key.home.arpa", targetB)
	if p3 == p1 {
		t.Fatal("expected happy-path proxy to refresh when target changes")
	}
}

func TestProxyServerHappyPathProxyCacheEvictsLeastRecentlyUsed(t *testing.T) {
	table := newProxyTable()
	server := newProxyServer(table, nil, true, "home.arpa")
	server.happyProxyMaxEntries = 2

	target, err := parseTarget("http://127.0.0.1:4200")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	_ = server.happyPathProxy("a.home.arpa", target)
	_ = server.happyPathProxy("b.home.arpa", target)
	// Touch "a" so "b" becomes least-recently-used.
	_ = server.happyPathProxy("a.home.arpa", target)
	_ = server.happyPathProxy("c.home.arpa", target)

	server.happyProxyMu.RLock()
	defer server.happyProxyMu.RUnlock()
	if len(server.happyProxy) != 2 {
		t.Fatalf("expected cache size 2, got %d", len(server.happyProxy))
	}
	if _, ok := server.happyProxy["a.home.arpa"]; !ok {
		t.Fatal("expected a.home.arpa to remain in cache")
	}
	if _, ok := server.happyProxy["c.home.arpa"]; !ok {
		t.Fatal("expected c.home.arpa to be added to cache")
	}
	if _, ok := server.happyProxy["b.home.arpa"]; ok {
		t.Fatal("expected b.home.arpa to be evicted as least recently used")
	}
}

func TestAPIRouterDeleteInvalidatesHappyPathProxyCache(t *testing.T) {
	prevSuffix := hostnameSuffix
	hostnameSuffix = "home.arpa"
	t.Cleanup(func() { hostnameSuffix = prevSuffix })

	table := newProxyTable()
	server := newProxyServer(table, nil, true, "home.arpa")

	target, err := parseTarget("http://127.0.0.1:4200")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	table.set("api-key.home.arpa", target)
	_ = server.happyPathProxy("api-key.home.arpa", target)

	server.happyProxyMu.RLock()
	_, hadCache := server.happyProxy["api-key.home.arpa"]
	server.happyProxyMu.RUnlock()
	if !hadCache {
		t.Fatal("expected cached happy-path proxy before delete")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/routes/api-key.home.arpa", nil)
	rec := httptest.NewRecorder()
	apiRouter(table, server).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, rec.Code)
	}

	server.happyProxyMu.RLock()
	_, stillCached := server.happyProxy["api-key.home.arpa"]
	server.happyProxyMu.RUnlock()
	if stillCached {
		t.Fatal("expected cache entry to be removed on admin delete")
	}
}

func TestRouteHealerCoalescedWaitersIgnoreLeaderCancel(t *testing.T) {
	info := &containerInspect{}
	info.State.Running = true
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{
		"bridge": {IPAddress: "172.17.0.8"},
	}

	inspector := &contextAwareInspector{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		info:    info,
	}
	healer := newRouteHealer(newProxyTable(), inspector, "home.arpa", 4200, true, time.Second)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()

	firstResult := make(chan error, 1)
	go func() {
		_, err := healer.Heal(firstCtx, "api-key.home.arpa")
		firstResult <- err
	}()

	<-inspector.entered

	secondResult := make(chan error, 1)
	go func() {
		_, err := healer.Heal(context.Background(), "api-key.home.arpa")
		secondResult <- err
	}()

	// Cancel the leader request while heal is in-flight.
	cancelFirst()
	close(inspector.release)

	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("expected coalesced waiter to succeed despite leader cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced waiter timed out")
	}

	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("expected leader to complete with shared result, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader timed out")
	}
}

func TestRouteHealerPanicCleansInflightEntry(t *testing.T) {
	inspector := &panicInspector{
		entered: make(chan struct{}),
	}
	healer := newRouteHealer(newProxyTable(), inspector, "home.arpa", 4200, true, time.Second)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		defer func() { _ = recover() }()
		_, _ = healer.Heal(context.Background(), "api-key.home.arpa")
	}()

	<-inspector.entered
	<-firstDone

	healer.mu.Lock()
	_, ok := healer.inflight["api-key.home.arpa"]
	healer.mu.Unlock()
	if ok {
		t.Fatal("expected inflight entry to be removed after panic")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		defer func() { _ = recover() }()
		_, _ = healer.Heal(context.Background(), "api-key.home.arpa")
	}()

	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second call blocked; stale inflight entry likely remained after panic")
	}
}
