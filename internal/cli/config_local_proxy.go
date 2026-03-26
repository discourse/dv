package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/xdg"
)

var configLocalProxyCmd = &cobra.Command{
	Use:   "local-proxy",
	Short: "Run a local proxy so containers are reachable via NAME.dv.localhost",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		// Handle --remove flag
		removeFlag, _ := cmd.Flags().GetBool("remove")
		if removeFlag {
			lp := cfg.LocalProxy
			lp.ApplyDefaults()

			if docker.Exists(lp.ContainerName) {
				if docker.Running(lp.ContainerName) {
					fmt.Fprintf(cmd.OutOrStdout(), "Stopping local proxy container '%s'...\n", lp.ContainerName)
					if err := docker.Stop(lp.ContainerName); err != nil {
						return fmt.Errorf("failed to stop container: %w", err)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removing local proxy container '%s'...\n", lp.ContainerName)
				if err := docker.Remove(lp.ContainerName); err != nil {
					return fmt.Errorf("failed to remove container: %w", err)
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Local proxy container '%s' does not exist.\n", lp.ContainerName)
			}

			if docker.ImageExists(lp.ImageTag) {
				fmt.Fprintf(cmd.OutOrStdout(), "Removing local proxy image '%s'...\n", lp.ImageTag)
				if err := docker.RemoveImage(lp.ImageTag); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to remove image: %v\n", err)
				}
			}

			cfg.LocalProxy.Enabled = false
			if err := config.Save(configDir, cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Local proxy removed.")
			return nil
		}

		prev := cfg.LocalProxy
		prev.ApplyDefaults()
		lp := prev

		nameFlag, _ := cmd.Flags().GetString("name")
		imageFlag, _ := cmd.Flags().GetString("image")
		hostnameFlag, _ := cmd.Flags().GetString("hostname")
		httpPortFlag, _ := cmd.Flags().GetInt("http-port")
		httpsPortFlag, _ := cmd.Flags().GetInt("https-port")
		apiPortFlag, _ := cmd.Flags().GetInt("api-port")
		rebuild, _ := cmd.Flags().GetBool("rebuild")
		recreate, _ := cmd.Flags().GetBool("recreate")
		public, _ := cmd.Flags().GetBool("public")
		httpsEnabled, _ := cmd.Flags().GetBool("https")
		publicChanged := cmd.Flags().Changed("public")
		hostnameChanged := cmd.Flags().Changed("hostname")

		if name := trimFlag(nameFlag); name != "" {
			lp.ContainerName = name
		}
		if img := trimFlag(imageFlag); img != "" {
			lp.ImageTag = img
		}
		if hostnameChanged {
			lp.Hostname = trimFlag(hostnameFlag)
		}
		if httpPortFlag > 0 {
			lp.HTTPPort = httpPortFlag
		}
		if httpsPortFlag > 0 {
			lp.HTTPSPort = httpsPortFlag
		}
		if apiPortFlag > 0 {
			lp.APIPort = apiPortFlag
		}
		// HTTPS is always off by default, must explicitly pass --https to enable
		lp.HTTPS = httpsEnabled
		if publicChanged {
			lp.Public = public
		}
		lp.ApplyDefaults()

		if lp.HTTPPort == lp.APIPort {
			return fmt.Errorf("http-port and api-port must differ")
		}
		if lp.HTTPS && lp.HTTPSPort == lp.APIPort {
			return fmt.Errorf("https-port and api-port must differ")
		}
		if lp.HTTPS && lp.HTTPSPort == lp.HTTPPort {
			return fmt.Errorf("https-port and http-port must differ")
		}

		if lp.HTTPS {
			// The proxy image needs the latest embedded assets to support HTTPS.
			rebuild = true
		}

		if rebuild || !docker.ImageExists(lp.ImageTag) {
			fmt.Fprintf(cmd.OutOrStdout(), "Building local proxy image '%s'...\n", lp.ImageTag)
			if err := localproxy.BuildImage(configDir, lp); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Reusing existing image '%s'.\n", lp.ImageTag)
		}

		if docker.Exists(lp.ContainerName) && localProxySettingsChanged(prev, lp) {
			recreate = true
		}

		if lp.HTTPS {
			if err := localproxy.EnsureMKCertTLS(configDir, lp.Hostname); err != nil {
				return err
			}
		}

		if err := localproxy.EnsureContainer(configDir, lp, recreate); err != nil {
			return err
		}
		if err := localproxy.Healthy(lp, 5*time.Second); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
		}

		lp.Enabled = true
		cfg.LocalProxy = lp
		if err := config.Save(configDir, cfg); err != nil {
			return err
		}

		if lp.Public {
			if lp.HTTPS {
				fmt.Fprintf(cmd.OutOrStdout(), "Local proxy '%s' is ready on port %d (HTTP→HTTPS redirect), %d (HTTPS) (public); API on %d (public).\n", lp.ContainerName, lp.HTTPPort, lp.HTTPSPort, lp.APIPort)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Local proxy '%s' is ready on port %d (public); API on %d (public).\n", lp.ContainerName, lp.HTTPPort, lp.APIPort)
			}
		} else {
			if lp.HTTPS {
				fmt.Fprintf(cmd.OutOrStdout(), "Local proxy '%s' is ready on port %d (HTTP→HTTPS redirect), %d (HTTPS) (localhost only); API on %d (localhost only).\n", lp.ContainerName, lp.HTTPPort, lp.HTTPSPort, lp.APIPort)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Local proxy '%s' is ready on port %d (localhost only); API on %d (localhost only).\n", lp.ContainerName, lp.HTTPPort, lp.APIPort)
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "New containers will register as NAME.%s when this proxy is running. Remove the proxy container to stop using it.\n", lp.Hostname)
		return nil
	},
}

func init() {
	configLocalProxyCmd.Flags().String("name", "", "Container name to run the proxy as (default dv-local-proxy)")
	configLocalProxyCmd.Flags().String("image", "", "Image tag to build/use for the proxy (default dv-local-proxy)")
	configLocalProxyCmd.Flags().String("hostname", "", "Base hostname for containers (default dv.localhost, containers become NAME.hostname)")
	configLocalProxyCmd.Flags().Int("http-port", 0, "Host port that will listen for NAME.dv.localhost requests (defaults to 80)")
	configLocalProxyCmd.Flags().Bool("https", false, "Enable HTTPS for NAME.dv.localhost using mkcert and redirect HTTP to HTTPS")
	configLocalProxyCmd.Flags().Int("https-port", 0, "Host port that will listen for HTTPS NAME.dv.localhost requests (defaults to 443 when --https is enabled)")
	configLocalProxyCmd.Flags().Int("api-port", 0, "Host port for the proxy management API")
	configLocalProxyCmd.Flags().Bool("rebuild", false, "Force rebuilding the proxy image even if it exists")
	configLocalProxyCmd.Flags().Bool("recreate", false, "Remove any existing proxy container before starting")
	configLocalProxyCmd.Flags().Bool("public", false, "Listen on all network interfaces (default: private/localhost only)")
	configLocalProxyCmd.Flags().Bool("remove", false, "Stop and remove the local proxy container and image")
	configCmd.AddCommand(configLocalProxyCmd)
}

func trimFlag(val string) string {
	return strings.TrimSpace(val)
}

func localProxySettingsChanged(prev config.LocalProxyConfig, next config.LocalProxyConfig) bool {
	// ContainerName/ImageTag changes are intentionally not treated as in-place updates.
	return prev.HTTPPort != next.HTTPPort ||
		prev.HTTPS != next.HTTPS ||
		prev.HTTPSPort != next.HTTPSPort ||
		prev.APIPort != next.APIPort ||
		prev.Public != next.Public ||
		prev.Hostname != next.Hostname
}
