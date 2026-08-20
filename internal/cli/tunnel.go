package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

var tunnelCmd = &cobra.Command{
	Use:   "tunnel PORT",
	Short: "Tunnel a container TCP port to localhost",
	Long: `Forward a TCP port from the selected container to an IPv4 host address
without recreating the container. The tunnel remains active until interrupted.

The bind target defaults to 127.0.0.1. Use --bind all, an IPv4 address, or an
interface name to make it reachable elsewhere. The container must have socat
installed.`,
	Example: `  dv tunnel 5432
  dv tunnel 3001 --host-port 13001
  dv tunnel 8080 --bind all
  dv tunnel 8080 --bind en0
  dv tunnel 5432 --name my-agent`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnelCommand,
}

func runTunnelCommand(cmd *cobra.Command, args []string) error {
	containerPort, requestedHostPort, autoHostPort, err := resolveTunnelPorts(args[0], cmd)
	if err != nil {
		return err
	}
	bindAddress, err := resolveTunnelBind(cmd)
	if err != nil {
		return err
	}

	target, err := resolveContainerTarget(cmd)
	if err != nil {
		return err
	}
	if !docker.Exists(target.name) {
		return fmt.Errorf("container '%s' does not exist; create it with 'dv start'", target.name)
	}
	if !docker.Running(target.name) {
		return fmt.Errorf("container '%s' is not running; start it with 'dv start'", target.name)
	}

	if err := ensureContainerSocat(cmd.Context(), target.name); err != nil {
		return err
	}

	attempts := 1
	if autoHostPort {
		attempts = automaticTunnelPortAttempts
	}
	listener, hostPort, err := listenTunnelHostPort(bindAddress, requestedHostPort, attempts, net.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	if hostPort != requestedHostPort {
		fmt.Fprintf(cmd.OutOrStdout(), "Port %d is in use on %s, using %d.\n", requestedHostPort, bindAddress, hostPort)
	}
	if ip := net.ParseIP(bindAddress); ip != nil && !ip.IsLoopback() {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: tunnel is bound to %s and may be reachable from your network.\n", bindAddress)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(cmd.OutOrStdout(), "Tunneling %s:%d to %s:127.0.0.1:%d\n", bindAddress, hostPort, target.name, containerPort)
	fmt.Fprintln(cmd.OutOrStdout(), "Press Ctrl+C to stop.")

	err = runTunnelServer(ctx, listener, target.name, containerPort, cmd.ErrOrStderr(), runDockerTunnelConnection)
	if ctx.Err() != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Tunnel stopped.")
		return nil
	}
	return err
}

func resolveTunnelPorts(containerPortArg string, cmd *cobra.Command) (int, int, bool, error) {
	containerPort, err := parseTunnelPort(containerPortArg, "container port")
	if err != nil {
		return 0, 0, false, err
	}
	hostPortValue, err := cmd.Flags().GetString("host-port")
	if err != nil {
		return 0, 0, false, fmt.Errorf("read host port: %w", err)
	}
	hostPortValue = strings.TrimSpace(hostPortValue)
	if hostPortValue == "" || strings.EqualFold(hostPortValue, "auto") {
		return containerPort, containerPort, true, nil
	}
	hostPort, err := parseTunnelPort(hostPortValue, "host port")
	if err != nil {
		return 0, 0, false, err
	}
	return containerPort, hostPort, false, nil
}

func resolveTunnelBind(cmd *cobra.Command) (string, error) {
	value, err := cmd.Flags().GetString("bind")
	if err != nil {
		return "", fmt.Errorf("read bind target: %w", err)
	}
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "localhost":
		return "127.0.0.1", nil
	case "all", "*":
		return "0.0.0.0", nil
	}
	if ip := net.ParseIP(value); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
		return "", fmt.Errorf("bind target %q is IPv6; dv tunnel currently supports IPv4 only", value)
	}

	iface, err := net.InterfaceByName(value)
	if err != nil {
		return "", fmt.Errorf("bind target %q is not an IPv4 address or network interface: %w", value, err)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("list addresses for interface %q: %w", value, err)
	}
	for _, address := range addresses {
		var ip net.IP
		switch address := address.(type) {
		case *net.IPNet:
			ip = address.IP
		case *net.IPAddr:
			ip = address.IP
		}
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
	}
	return "", fmt.Errorf("network interface %q has no IPv4 address", value)
}

func parseTunnelPort(value, label string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be a number from 1 to 65535", label, value)
	}
	if err := validateTCPPort(port, label); err != nil {
		return 0, err
	}
	return port, nil
}

func init() {
	tunnelCmd.Flags().String("bind", "127.0.0.1", "IPv4 address, interface name, or all")
	tunnelCmd.Flags().String("host-port", "auto", "Host port, or auto to try PORT and the next 19 ports")
	tunnelCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
}
