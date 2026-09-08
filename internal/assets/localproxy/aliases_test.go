package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAliasRecoveryAndOwnershipChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aliases.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"customers.dev.home.arpa":"forum"}`)
	info := &containerInspect{}
	info.State.Running = true
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"bridge": {IPAddress: "172.17.0.8"}}
	inspector := &fakeInspector{info: info}
	table := newProxyTable()
	healer := newRouteHealer(table, inspector, "dev.home.arpa", 3000, true, time.Second)
	healer.aliasesFile = path
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	if _, err := healer.Heal(context.Background(), "customers.dev.home.arpa"); err != nil {
		t.Fatal(err)
	}
	if inspector.last != "forum" {
		t.Fatalf("inspected %q", inspector.last)
	}
	write(`{"customers.dev.home.arpa":"renamed"}`)
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	if table.lookup("customers.dev.home.arpa") != nil {
		t.Fatal("stale target after rename")
	}
	if _, err := healer.Heal(context.Background(), "customers.dev.home.arpa"); err != nil {
		t.Fatal(err)
	}
	if inspector.last != "renamed" {
		t.Fatalf("inspected %q", inspector.last)
	}
	write(`{}`)
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	if table.lookup("customers.dev.home.arpa") != nil {
		t.Fatal("stale target after removal")
	}
	write(`not-json`)
	if err := healer.refreshAliases(); err == nil {
		t.Fatal("accepted corrupt ownership file")
	}
}

func TestAliasProxyPreservesHost(t *testing.T) {
	previous := hostnameSuffix
	hostnameSuffix = "dev.home.arpa"
	t.Cleanup(func() { hostnameSuffix = previous })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Host) }))
	defer backend.Close()
	table := newProxyTable()
	target, err := parseTarget(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	table.set("customers.dev.home.arpa", target)
	healer := newRouteHealer(table, nil, "dev.home.arpa", 3000, false, time.Second)
	path := filepath.Join(t.TempDir(), "aliases.json")
	if err := os.WriteFile(path, []byte(`{"customers.dev.home.arpa":"forum"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	healer.aliasesFile = path
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	table.set("customers.dev.home.arpa", target)
	server := newProxyServer(table, healer, false, "dev.home.arpa")
	req := httptest.NewRequest("GET", "http://customers.dev.home.arpa/", nil)
	out := httptest.NewRecorder()
	server.ServeHTTP(out, req)
	if out.Code != http.StatusOK || out.Body.String() != "customers.dev.home.arpa" {
		t.Fatalf("%d: %s", out.Code, out.Body.String())
	}
}

func TestAliasFileFailureDoesNotBreakPrimary(t *testing.T) {
	previous := hostnameSuffix
	hostnameSuffix = "dev.home.arpa"
	t.Cleanup(func() { hostnameSuffix = previous })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Host) }))
	defer backend.Close()
	table := newProxyTable()
	target, _ := parseTarget(backend.URL)
	table.set("forum.dev.home.arpa", target)
	table.set("customers.dev.home.arpa", target)
	healer := newRouteHealer(table, nil, "dev.home.arpa", 3000, false, time.Second)
	healer.aliasesFile = filepath.Join(t.TempDir(), "aliases.json")
	if err := os.WriteFile(healer.aliasesFile, []byte(`{"customers.dev.home.arpa":"forum"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	server := newProxyServer(table, healer, false, "dev.home.arpa")
	for _, missing := range []bool{false, true} {
		if missing {
			if err := os.Remove(healer.aliasesFile); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(healer.aliasesFile, []byte("invalid"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			host   string
			status int
		}{{"forum", 200}, {"customers", 502}} {
			out := httptest.NewRecorder()
			server.ServeHTTP(out, httptest.NewRequest("GET", "http://"+tc.host+".dev.home.arpa/", nil))
			if out.Code != tc.status {
				t.Fatalf("missing=%v %s: %d", missing, tc.host, out.Code)
			}
		}
	}
}

func TestAliasRegistrationAfterOwnershipChangeWithoutAutoHeal(t *testing.T) {
	previous := hostnameSuffix
	hostnameSuffix = "dev.home.arpa"
	t.Cleanup(func() { hostnameSuffix = previous })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer backend.Close()
	table := newProxyTable()
	healer := newRouteHealer(table, nil, "dev.home.arpa", 3000, false, time.Second)
	healer.aliasesFile = filepath.Join(t.TempDir(), "aliases.json")
	write := func(owner string) {
		t.Helper()
		// Atomic replacement with same-size JSON must invalidate cached ownership.
		tmp := healer.aliasesFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(fmt.Sprintf(`{"customers.dev.home.arpa":%q}`, owner)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, healer.aliasesFile); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	write("new")
	server := newProxyServer(table, healer, false, "dev.home.arpa")
	out := httptest.NewRecorder()
	payload := fmt.Sprintf(`{"host":"customers.dev.home.arpa","target":%q}`, backend.URL)
	apiRouter(table, server).ServeHTTP(out, httptest.NewRequest("POST", "/api/routes", strings.NewReader(payload)))
	if out.Code != 201 {
		t.Fatalf("register: %d %s", out.Code, out.Body.String())
	}
	out = httptest.NewRecorder()
	server.ServeHTTP(out, httptest.NewRequest("GET", "http://customers.dev.home.arpa/", nil))
	if out.Code != 200 {
		t.Fatalf("fresh registration invalidated: %d", out.Code)
	}
	if healer.aliasOwner("customers.dev.home.arpa") != "new" {
		t.Fatal("stale ownership")
	}
}

func TestNewAliasClaimInvalidatesStalePrimaryRoute(t *testing.T) {
	table := newProxyTable()
	target, _ := parseTarget("http://127.0.0.1:3000")
	table.set("customers.dev.home.arpa", target)
	healer := newRouteHealer(table, nil, "dev.home.arpa", 3000, false, time.Second)
	healer.aliasesFile = filepath.Join(t.TempDir(), "aliases.json")
	if err := os.WriteFile(healer.aliasesFile, []byte(`{"customers.dev.home.arpa":"forum"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := healer.refreshAliases(); err != nil {
		t.Fatal(err)
	}
	if table.lookup("customers.dev.home.arpa") != nil {
		t.Fatal("new alias retained stale primary target")
	}
}

func TestPrimaryRegistrationSurvivesAliasFileFailure(t *testing.T) {
	previous := hostnameSuffix
	hostnameSuffix = "dev.home.arpa"
	t.Cleanup(func() { hostnameSuffix = previous })
	table := newProxyTable()
	healer := newRouteHealer(table, nil, "dev.home.arpa", 3000, false, time.Second)
	healer.aliasesFile = filepath.Join(t.TempDir(), "missing.json")
	healer.aliases = map[string]string{"customers.dev.home.arpa": "forum"}
	server := newProxyServer(table, healer, false, "dev.home.arpa")
	for _, tc := range []struct {
		host string
		want int
	}{{"forum", 201}, {"customers", 503}} {
		out := httptest.NewRecorder()
		body := fmt.Sprintf(`{"host":"%s.dev.home.arpa","target":"http://127.0.0.1:3000"}`, tc.host)
		apiRouter(table, server).ServeHTTP(out, httptest.NewRequest("POST", "/api/routes", strings.NewReader(body)))
		if out.Code != tc.want {
			t.Fatalf("%s status %d", tc.host, out.Code)
		}
	}
}
