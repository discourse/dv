package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

var mailCmd = &cobra.Command{
	Use:   "mail [--port PORT] [--host-port HOST_PORT]",
	Short: "Run MailHog and tunnel it to localhost",
	Long: `Start MailHog in the container and create a tunnel to localhost.
This allows you to access MailHog from your browser without reconfiguring Docker.
Press Ctrl+C to stop both MailHog and the tunnel.`,
	Args: cobra.NoArgs,
	RunE: runMailCommand,
}

func runMailCommand(cmd *cobra.Command, _ []string) error {
	verbose, _ := cmd.Flags().GetBool("verbose")
	log := func(format string, a ...any) {
		if verbose {
			fmt.Fprintf(cmd.OutOrStdout(), "[debug] "+format+"\n", a...)
		}
	}

	target, err := resolveContainerTarget(cmd)
	if err != nil {
		return err
	}
	name := target.name
	log("Using container: %s", name)
	if !docker.Exists(name) {
		return fmt.Errorf("container '%s' does not exist; create it with 'dv start'", name)
	}
	if !docker.Running(name) {
		return fmt.Errorf("container '%s' is not running; start it with 'dv start'", name)
	}

	containerPort, _ := cmd.Flags().GetInt("port")
	if containerPort == 0 {
		containerPort = 8025
	}
	if err := validateTCPPort(containerPort, "container port"); err != nil {
		return err
	}
	hostPort, _ := cmd.Flags().GetInt("host-port")
	if hostPort == 0 {
		hostPort = containerPort
	}
	if err := validateTCPPort(hostPort, "host port"); err != nil {
		return err
	}
	log("Container port: %d, Host port: %d", containerPort, hostPort)

	if err := ensureContainerSocat(cmd.Context(), name); err != nil {
		return err
	}
	listener, hostPort, err := listenTunnelHostPort("127.0.0.1", hostPort, 1, net.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(ctx)

	log("Starting MailHog process: docker exec -u discourse %s mailhog", name)
	mailhogProcess := exec.CommandContext(runCtx, "docker", "exec", "-u", "discourse", name, "mailhog")
	mailhogProcess.Stderr = cmd.ErrOrStderr()
	if err := mailhogProcess.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start MailHog: %w", err)
	}
	log("MailHog started with PID: %d", mailhogProcess.Process.Pid)

	mailhogDone := make(chan error, 1)
	go func() {
		mailhogDone <- mailhogProcess.Wait()
	}()
	tunnelDone := make(chan error, 1)
	go func() {
		tunnelDone <- runTunnelServer(runCtx, listener, name, containerPort, cmd.ErrOrStderr(), runDockerTunnelConnection)
	}()

	mailhogWaited := false
	tunnelWaited := false
	defer func() {
		cancel()
		log("Cleanup: killing mailhog inside container")
		killCmd := exec.Command("docker", "exec", name, "pkill", "-f", "mailhog")
		if err := killCmd.Run(); err != nil {
			log("pkill mailhog returned: %v", err)
		}
		if !mailhogWaited {
			<-mailhogDone
		}
		if !tunnelWaited {
			<-tunnelDone
		}
	}()

	fmt.Fprintln(cmd.OutOrStdout(), "Starting MailHog...")
	fmt.Fprintln(cmd.OutOrStdout(), "✓ MailHog is running and tunneled to localhost")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintf(cmd.OutOrStdout(), "  Open in your browser: http://localhost:%d\n", hostPort)
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "  Press Ctrl+C to stop")

	select {
	case <-ctx.Done():
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "Stopping MailHog and tunnel...")
		return nil
	case err := <-mailhogDone:
		mailhogWaited = true
		if err != nil && runCtx.Err() == nil {
			return fmt.Errorf("MailHog exited: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "MailHog exited")
		return nil
	case err := <-tunnelDone:
		tunnelWaited = true
		if err != nil {
			return fmt.Errorf("MailHog tunnel failed: %w", err)
		}
		return nil
	}
}

func init() {
	mailCmd.Flags().Int("port", 8025, "MailHog port inside the container")
	mailCmd.Flags().Int("host-port", 0, "Port to expose on localhost (defaults to same as --port)")
	mailCmd.Flags().BoolP("verbose", "V", false, "Enable verbose debug output")
}
