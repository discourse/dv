package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var pullCmd = &cobra.Command{
	Use:   "pull [IMAGE_NAME]",
	Short: "Pull a prebuilt dv Docker image instead of building locally",
	Args:  cobra.RangeArgs(0, 1),
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		overrideTag, _ := cmd.Flags().GetString("tag")
		removeExisting, _ := cmd.Flags().GetBool("rm-existing")

		// Resolve image name from args or use currently selected image
		imageName := cfg.SelectedImage
		if len(args) == 1 {
			imageName = args[0]
		}

		img, ok := cfg.Images[imageName]
		if !ok {
			return fmt.Errorf("unknown image '%s'", imageName)
		}

		// Determine which tag/ref to pull.
		// If --tag is specified, trust it as the ref (full name or tag).
		// Otherwise, for the stock discourse image use the official published image
		// "discourse/dv:latest" and retag locally to the
		// configured name so dv start/enter continue to work without
		// additional configuration.
		ref := img.Tag
		if overrideTag != "" {
			ref = overrideTag
		} else if img.Kind == "discourse" && img.Tag == "ai_agent" {
			// For the stock discourse image with default tag, use the
			// official prebuilt image, and then retag it locally so dv continues
			// to use the configured tag.
			ref = "discourse/dv:latest"
		}

		if ref == "" {
			return fmt.Errorf("no tag configured for image '%s'", imageName)
		}

		if removeExisting && docker.ImageExists(ref) {
			fmt.Fprintf(cmd.OutOrStdout(), "Removing existing image %s...\n", ref)
			if err := docker.RemoveImage(ref); err != nil {
				return err
			}
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Pulling Docker image: %s\n", ref)
		if err := docker.Pull(ref); err != nil {
			return err
		}

		// If we pulled the official image for the default stock config, retag
		// it to the configured tag so existing workflows (dv start, etc.) work
		// without requiring users to change their config.
		if img.Kind == "discourse" && img.Tag != "" && ref == "discourse/dv:latest" && img.Tag != ref {
			fmt.Fprintf(cmd.OutOrStdout(), "Tagging %s as %s\n", ref, img.Tag)
			if err := docker.TagImage(ref, img.Tag); err != nil {
				return err
			}
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Done.")
		return nil
	},
}

func init() {
	pullCmd.Flags().String("tag", "", "Override the Docker image tag or ref to pull")
	pullCmd.Flags().Bool("rm-existing", false, "Remove existing local image with the same tag before pulling")
}
