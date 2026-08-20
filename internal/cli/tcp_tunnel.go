package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"dv/internal/docker"
)

const automaticTunnelPortAttempts = 20

func validateTCPPort(port int, label string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid %s %d: must be from 1 to 65535", label, port)
	}
	return nil
}

func ensureContainerSocat(ctx context.Context, name string) error {
	if _, err := docker.ExecOutputContext(ctx, name, "/", nil, []string{"sh", "-c", "command -v socat"}); err != nil {
		return fmt.Errorf("socat is required inside container '%s': %w", name, err)
	}
	return nil
}

type tunnelListenFunc func(network, address string) (net.Listener, error)

func listenTunnelHostPort(bindAddress string, startPort, attempts int, listen tunnelListenFunc) (net.Listener, int, error) {
	if attempts < 1 {
		attempts = 1
	}
	for offset := 0; offset < attempts; offset++ {
		port := startPort + offset
		if port > 65535 {
			break
		}
		listener, err := listen("tcp4", net.JoinHostPort(bindAddress, strconv.Itoa(port)))
		if err == nil {
			return listener, port, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, fmt.Errorf("cannot listen on %s:%d: %w", bindAddress, port, err)
		}
	}
	endPort := min(startPort+attempts-1, 65535)
	if attempts == 1 {
		return nil, 0, fmt.Errorf("port %d is already in use on %s; choose another host port", startPort, bindAddress)
	}
	return nil, 0, fmt.Errorf("no available host port on %s from %d through %d", bindAddress, startPort, endPort)
}

type tunnelConnectionRunner func(context.Context, string, int, io.ReadWriteCloser, io.Writer) error

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func runTunnelServer(ctx context.Context, listener net.Listener, name string, containerPort int, errOut io.Writer, run tunnelConnectionRunner) error {
	var connections sync.WaitGroup
	defer connections.Wait()
	connectionErrOut := &synchronizedWriter{w: errOut}

	serverDone := make(chan struct{})
	defer close(serverDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-serverDone:
		}
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept tunnel connection: %w", err)
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer connection.Close()
			if err := run(ctx, name, containerPort, connection, connectionErrOut); err != nil && ctx.Err() == nil {
				fmt.Fprintf(connectionErrOut, "Tunnel connection failed: %v\n", err)
			}
		}()
	}
}

func runDockerTunnelConnection(ctx context.Context, name string, containerPort int, connection io.ReadWriteCloser, errOut io.Writer) error {
	command := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", "discourse", name, "socat", "-t", "30", "STDIO", fmt.Sprintf("TCP:127.0.0.1:%d", containerPort))
	return relayTunnelConnection(command, connection, errOut)
}

func relayTunnelConnection(command *exec.Cmd, connection io.ReadWriteCloser, errOut io.Writer) error {
	command.Stderr = errOut

	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}

	inputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdin, connection)
		_ = stdin.Close()
		close(inputDone)
	}()

	// Drain stdout completely before Wait closes its pipe. Once the remote side
	// reaches EOF, close the client connection to unblock the input copier too.
	_, _ = io.Copy(connection, stdout)
	_ = connection.Close()
	<-inputDone
	return command.Wait()
}
