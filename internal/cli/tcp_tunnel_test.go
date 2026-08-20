package cli

import (
	"context"
	"io"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestListenTunnelHostPortTriesConsecutivePorts(t *testing.T) {
	calls := 0
	listen := func(_, _ string) (net.Listener, error) {
		calls++
		if calls < 3 {
			return nil, syscall.EADDRINUSE
		}
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	listener, port, err := listenTunnelHostPort("127.0.0.1", 5432, automaticTunnelPortAttempts, listen)
	if err != nil {
		t.Fatalf("listenTunnelHostPort() error = %v", err)
	}
	defer listener.Close()
	if port != 5434 || calls != 3 {
		t.Fatalf("listenTunnelHostPort() = port %d after %d calls, want 5434 after 3", port, calls)
	}
}

func TestListenTunnelHostPortStopsAfterMaximumAttempts(t *testing.T) {
	calls := 0
	listen := func(_, _ string) (net.Listener, error) {
		calls++
		return nil, syscall.EADDRINUSE
	}
	if _, _, err := listenTunnelHostPort("127.0.0.1", 5432, automaticTunnelPortAttempts, listen); err == nil {
		t.Fatal("listenTunnelHostPort() error = nil, want error")
	}
	if calls != automaticTunnelPortAttempts {
		t.Fatalf("listen attempts = %d, want %d", calls, automaticTunnelPortAttempts)
	}
}

func TestRunTunnelServerForwardsConnectionAndStops(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := func(_ context.Context, name string, port int, connection io.ReadWriteCloser, _ io.Writer) error {
		if name != "agent" || port != 5432 {
			t.Errorf("runner target = %s:%d, want agent:5432", name, port)
		}
		_, err := connection.Write([]byte("connected"))
		return err
	}
	go func() {
		done <- runTunnelServer(ctx, listener, "agent", 5432, io.Discard, runner)
	}()

	connection, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response := make([]byte, len("connected"))
	if _, err := io.ReadFull(connection, response); err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = connection.Close()
	if string(response) != "connected" {
		t.Fatalf("response = %q, want connected", response)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runTunnelServer() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTunnelServer did not stop after cancellation")
	}
}

func TestRelayTunnelConnectionDrainsOutputAfterClientHalfClose(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptTCP()
		if err != nil {
			done <- err
			return
		}
		done <- relayTunnelConnection(exec.Command("cat"), connection, io.Discard)
	}()

	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("tail-bytes-", 4096)
	if _, err := client.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if string(response) != payload {
		t.Fatalf("response length = %d, want %d", len(response), len(payload))
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relayTunnelConnection() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayTunnelConnection did not stop")
	}
}
