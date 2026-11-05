package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestStartupRequiresActualHTTPReadiness(t *testing.T) {
	for _, status := range []int{200, 503, 302} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" {
				t.Errorf("wrong probe path: %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		err := confirmHTTPReady(context.Background(), s.Listener.Addr().String())
		s.Close()
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}

func TestStartupReadinessUsesIPv4ForConfiguredIPv4Wildcard(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "127.0.0.1:") {
			t.Errorf("IPv4 configuration probed a different address family: %s", r.Host)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	_, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"0.0.0.0", ""} {
		if err := confirmHTTPReady(context.Background(), net.JoinHostPort(host, port)); err != nil {
			t.Fatalf("IPv4-only server must be reachable for host %q: %v", host, err)
		}
	}
}

func TestStartupReadinessPreservesExplicitIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	_ = s.Listener.Close()
	s.Listener = listener
	s.Start()
	defer s.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if err := confirmHTTPReady(context.Background(), net.JoinHostPort("::", port)); err != nil {
		t.Fatal(err)
	}
}

func TestStartupReadinessIgnoresSharedProxyTransport(t *testing.T) {
	previous := http.DefaultTransport
	proxyTransport := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		return nil, errors.New("shared proxy is unavailable")
	}}
	http.DefaultTransport = proxyTransport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		proxyTransport.CloseIdleConnections()
	})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	if err := confirmHTTPReady(context.Background(), s.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
}

func TestStartupReadinessRetainsConnectionFailureDetails(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	err = confirmHTTPReady(context.Background(), address)
	var connectionError *net.OpError
	if !errors.As(err, &connectionError) || !strings.Contains(err.Error(), address) {
		t.Fatalf("connection error must retain probe address and original cause: %v", err)
	}
}
