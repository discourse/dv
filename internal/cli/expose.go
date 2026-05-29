package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var exposeCmd = &cobra.Command{
	Use:   "expose [--port PORT]",
	Short: "Expose container on public network interfaces for testing",
	Long: `Expose the container on all non-localhost network interfaces.
This allows you to access the container from other devices on your local network (e.g., iPhone).
Press Ctrl+C to stop exposing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		verbose, _ := cmd.Flags().GetBool("verbose")
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		name := currentAgentName(cfg)
		if verbose {
			fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Selected container: %s\n", name)
		}
		if !docker.Running(name) {
			return fmt.Errorf("container '%s' is not running; start it with 'dv start'", name)
		}
		if verbose {
			fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Container '%s' is running\n", name)
		}

		// Get the container target (IP and port)
		portOverride, _ := cmd.Flags().GetInt("port")
		var targetAddr string
		var containerPort int
		if portOverride > 0 {
			// If port override, assume localhost and default HTTP
			targetAddr = fmt.Sprintf("localhost:%d", portOverride)
			containerPort = portOverride
			if verbose {
				fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Using port override, target: %s\n", targetAddr)
			}
		} else {
			// Get the container's IP and port directly
			if verbose {
				fmt.Fprintln(cmd.OutOrStdout(), "[verbose] Querying container target...")
			}
			var containerIP string
			var err error
			containerIP, containerPort, err = getContainerTarget(name, cfg, verbose, cmd.OutOrStdout())
			if err != nil {
				return fmt.Errorf("failed to get container target: %w", err)
			}
			targetAddr = fmt.Sprintf("%s:%d", containerIP, containerPort)
			if verbose {
				fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Will proxy to %s\n", targetAddr)
			}
		}

		// Get all non-localhost network interfaces
		if verbose {
			fmt.Fprintln(cmd.OutOrStdout(), "[verbose] Scanning network interfaces...")
		}
		ips, err := getNonLocalhostIPs()
		if err != nil {
			return err
		}
		if len(ips) == 0 {
			return fmt.Errorf("no non-localhost network interfaces found")
		}
		if verbose {
			fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Found %d non-localhost IP(s):\n", len(ips))
			for _, ip := range ips {
				ifaceName := getInterfaceName(ip)
				if ifaceName != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "[verbose]   %s (%s)\n", ip, ifaceName)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "[verbose]   %s\n", ip)
				}
			}
		}

		// Find an available port for all interfaces
		if verbose {
			fmt.Fprintln(cmd.OutOrStdout(), "[verbose] Searching for available port starting from 10000...")
		}
		availablePort, err := findAvailablePort(ips, verbose, cmd.OutOrStdout())
		if err != nil {
			return fmt.Errorf("failed to find available port: %w", err)
		}
		if verbose {
			fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Found available port: %d\n", availablePort)
			fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Will proxy %s:%d -> %s\n", ips[0], availablePort, targetAddr)
		}

		// Query Discourse hostname for URL rewriting
		fmt.Fprint(cmd.OutOrStdout(), "Querying Discourse hostname... ")
		discourseHostname, err := getDiscourseHostname(name, verbose, cmd.OutOrStdout())
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), "failed")
			return fmt.Errorf("failed to get Discourse hostname: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), discourseHostname)

		// Start proxies for each IP
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var wg sync.WaitGroup
		errChan := make(chan error, len(ips))

		for _, ip := range ips {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				if verbose {
					fmt.Fprintf(cmd.OutOrStdout(), "[verbose] Starting proxy on %s:%d\n", ip, availablePort)
				}
				if err := startHTTPProxy(ctx, ip, availablePort, targetAddr, discourseHostname, verbose, cmd.OutOrStdout()); err != nil {
					errChan <- fmt.Errorf("proxy on %s: %w", ip, err)
				}
			}(ip)
		}

		// Display success message
		fmt.Fprintln(cmd.OutOrStdout(), "✓ Container exposed on local network")
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "  From your device, visit:")
		scheme := "http"
		if containerPort == 443 {
			scheme = "https"
		}
		for _, ip := range ips {
			ifaceName := getInterfaceName(ip)
			fmt.Fprintf(cmd.OutOrStdout(), "  %s://%s:%d", scheme, ip, availablePort)
			if ifaceName != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " (%s)", ifaceName)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "  Press Ctrl+C to stop")

		// Wait for interrupt signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		select {
		case <-sigChan:
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "Stopping...")
			cancel()
		case err := <-errChan:
			cancel()
			wg.Wait()
			return err
		}

		wg.Wait()
		return nil
	},
}

func init() {
	exposeCmd.Flags().Int("port", 0, "Port to expose (defaults to container's mapped port)")
	exposeCmd.Flags().Bool("verbose", false, "Show detailed debugging information")
}

// getDiscourseHostname queries the Discourse container for its current hostname
func getDiscourseHostname(name string, verbose bool, out io.Writer) (string, error) {
	if verbose {
		fmt.Fprintln(out, "[verbose] Querying Discourse hostname...")
	}
	hostname, err := docker.ExecOutput(name, "/var/www/discourse", nil, []string{
		"bin/rails", "runner", "puts Discourse.current_hostname",
	})
	if err != nil {
		return "", fmt.Errorf("failed to get Discourse hostname: %w", err)
	}
	hostname = strings.TrimSpace(hostname)
	if verbose {
		fmt.Fprintf(out, "[verbose] Discourse hostname: %s\n", hostname)
	}
	return hostname, nil
}

// getContainerTarget returns the container's IP and internal port to connect to
func getContainerTarget(name string, cfg config.Config, verbose bool, out io.Writer) (string, int, error) {
	// Get the container's IP address
	containerIP, err := docker.ContainerIP(name)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get container IP: %w", err)
	}
	if verbose {
		fmt.Fprintf(out, "[verbose] Container IP: %s\n", containerIP)
	}

	// Get container port from image config
	imgCfg := cfg.Images[cfg.SelectedImage]
	containerPort := imgCfg.ContainerPort
	if containerPort == 0 {
		containerPort = 3000 // fallback default (Rails)
	}
	if verbose {
		fmt.Fprintf(out, "[verbose] Container port: %d\n", containerPort)
	}

	return containerIP, containerPort, nil
}

// getNonLocalhostIPs returns all IPv4 addresses that are not localhost
func getNonLocalhostIPs() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var ips []string
	for _, iface := range ifaces {
		// Skip down interfaces
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		// Skip loopback
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Only IPv4, not loopback
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}

			ips = append(ips, ip.String())
		}
	}

	return ips, nil
}

// findAvailablePort finds a port that's available on all given IPs
// Starts from port 10000 to avoid privileged ports and common conflicts.
func findAvailablePort(ips []string, verbose bool, out io.Writer) (int, error) {
	const startPort = 10000
	const maxAttempts = 100
	for port := startPort; port < startPort+maxAttempts; port++ {
		allAvailable := true
		for _, ip := range ips {
			addr := fmt.Sprintf("%s:%d", ip, port)
			listener, err := net.Listen("tcp", addr)
			if err != nil {
				if verbose {
					fmt.Fprintf(out, "[verbose] Port %d unavailable on %s: %v\n", port, ip, err)
				}
				allAvailable = false
				break
			}
			listener.Close()
		}
		if allAvailable {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available port found after %d attempts starting from %d", maxAttempts, startPort)
}

// getInterfaceName returns a friendly name for the interface with the given IP
func getInterfaceName(targetIP string) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip != nil && ip.String() == targetIP {
				// Return a friendly name based on common patterns
				name := iface.Name
				if strings.HasPrefix(name, "en") {
					return "Wi-Fi"
				} else if strings.HasPrefix(name, "eth") {
					return "Ethernet"
				}
				return name
			}
		}
	}

	return ""
}

// startHTTPProxy starts an HTTP reverse proxy from listenIP:listenPort to targetAddr
// with URL rewriting to convert absolute URLs to relative ones
func startHTTPProxy(ctx context.Context, listenIP string, listenPort int, targetAddr, discourseHostname string, verbose bool, out io.Writer) error {
	addr := fmt.Sprintf("%s:%d", listenIP, listenPort)

	targetURL := &url.URL{
		Scheme: "http",
		Host:   targetAddr,
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			// Preserve the Host header for Discourse routing
			req.Host = discourseHostname
			if verbose {
				fmt.Fprintf(out, "[verbose] Proxying %s %s -> %s\n", req.Method, req.URL.Path, targetAddr)
			}
		},
		ModifyResponse: createURLRewriter(discourseHostname, verbose, out),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if verbose {
				fmt.Fprintf(out, "[verbose] Proxy error: %v\n", err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	server := &http.Server{
		Addr:    addr,
		Handler: proxy,
	}

	if verbose {
		fmt.Fprintf(out, "[verbose] HTTP proxy listening on %s\n", addr)
	}

	// Handle graceful shutdown
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// createURLRewriter returns a ModifyResponse function that rewrites absolute URLs to relative
func createURLRewriter(hostname string, verbose bool, out io.Writer) func(*http.Response) error {
	// Patterns to replace - order matters (more specific first)
	patterns := [][]byte{
		[]byte("https://" + hostname),
		[]byte("http://" + hostname),
		[]byte("//" + hostname),
	}

	return func(resp *http.Response) error {
		contentType := resp.Header.Get("Content-Type")
		if !isTextContentType(contentType) {
			return nil
		}

		// Read the body, decompressing if gzipped
		body, wasGzipped, err := readResponseBody(resp)
		if err != nil {
			return err
		}

		if len(body) == 0 {
			return nil
		}

		// Rewrite URLs
		modified := body
		for _, pattern := range patterns {
			modified = bytes.ReplaceAll(modified, pattern, []byte(""))
		}

		if verbose && !bytes.Equal(body, modified) {
			fmt.Fprintf(out, "[verbose] Rewrote URLs in %s response (%d bytes)\n", contentType, len(body))
		}

		// Write the modified body back
		return writeResponseBody(resp, modified, wasGzipped)
	}
}

// isTextContentType returns true if the content type should be processed for URL rewriting
func isTextContentType(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") ||
		strings.Contains(ct, "application/json") ||
		strings.Contains(ct, "text/javascript") ||
		strings.Contains(ct, "application/javascript") ||
		strings.Contains(ct, "text/css")
}

// readResponseBody reads the response body, decompressing if gzipped
// Returns the body bytes, whether it was gzipped, and any error
func readResponseBody(resp *http.Response) ([]byte, bool, error) {
	if resp.Body == nil {
		return nil, false, nil
	}

	wasGzipped := resp.Header.Get("Content-Encoding") == "gzip"

	var reader io.Reader = resp.Body
	if wasGzipped {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, false, err
		}
		defer gzReader.Close()
		reader = gzReader
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, wasGzipped, err
	}

	resp.Body.Close()
	return body, wasGzipped, nil
}

// writeResponseBody writes the modified body back to the response
func writeResponseBody(resp *http.Response, body []byte, recompress bool) error {
	var finalBody []byte

	if recompress {
		var buf bytes.Buffer
		gzWriter := gzip.NewWriter(&buf)
		if _, err := gzWriter.Write(body); err != nil {
			return err
		}
		if err := gzWriter.Close(); err != nil {
			return err
		}
		finalBody = buf.Bytes()
	} else {
		finalBody = body
		// Remove Content-Encoding if it was set but we're not compressing
		resp.Header.Del("Content-Encoding")
	}

	resp.Body = io.NopCloser(bytes.NewReader(finalBody))
	resp.ContentLength = int64(len(finalBody))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(finalBody)))

	return nil
}
