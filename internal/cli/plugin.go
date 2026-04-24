package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage plugins in the selected Discourse agent",
}

var pluginAddCmd = &cobra.Command{
	Use:   "add [PLUGIN...]",
	Short: "Clone plugins into the selected agent's plugins directory",
	Long: `Clone one or more plugins into the selected agent's Discourse plugins directory.

PLUGIN accepts:
  discourse-kanban                         -> https://github.com/discourse/discourse-kanban.git
  discourse/discourse-kanban               -> https://github.com/discourse/discourse-kanban.git
  https://github.com/discourse/foo.git      -> exact URL
  git@github.com:discourse/foo.git         -> exact SSH URL`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, err := currentDiscourseContainerContext(cmd)
		if err != nil {
			return err
		}

		branch, _ := cmd.Flags().GetString("branch")
		skipMaintenance, _ := cmd.Flags().GetBool("skip-maintenance")

		plugins, err := resolvePluginSpecs(args)
		if err != nil {
			return err
		}
		if branch != "" {
			for i := range plugins {
				plugins[i].Branch = branch
			}
		}

		envs := collectEnvPassthrough(ctx.cfg)
		needsSSH := false
		for _, input := range args {
			if pluginSpecNeedsSSH(input) {
				needsSSH = true
				break
			}
		}
		if needsSSH {
			envs = append(envs, "SSH_AUTH_SOCK=/tmp/ssh-agent.sock")
			if err := setupContainerSSHForwarding(cmd, ctx.name, ctx.workdir, true); err != nil {
				return err
			}
		}

		err = func() error {
			if !skipMaintenance {
				cleanupServices := stopServicesForProvisioning(cmd, ctx.name, ctx.workdir)
				defer cleanupServices()
			}

			if err := installPlugins(cmd, ctx.name, ctx.workdir, envs, plugins); err != nil {
				return err
			}

			if !skipMaintenance {
				if err := runMaintenance(cmd, ctx.name, ctx.workdir, envs); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Done.")
		return nil
	},
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List plugin directories in the selected agent",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, err := currentDiscourseContainerContext(cmd)
		if err != nil {
			return err
		}
		out, err := docker.ExecOutput(ctx.name, ctx.workdir, nil, []string{"bash", "-lc", `if [ -d plugins ]; then find plugins -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort; fi`})
		if err != nil {
			return err
		}
		out = strings.TrimSpace(out)
		if out == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "No plugins found.")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
		return nil
	},
}

type discourseContainerContext struct {
	cfg     config.Config
	name    string
	workdir string
}

func currentDiscourseContainerContext(cmd *cobra.Command) (discourseContainerContext, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return discourseContainerContext{}, err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return discourseContainerContext{}, err
	}

	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		name = currentAgentName(cfg)
	}
	if strings.TrimSpace(name) == "" {
		return discourseContainerContext{}, fmt.Errorf("no agent selected")
	}
	if !docker.Running(name) {
		return discourseContainerContext{}, fmt.Errorf("container '%s' is not running; run 'dv start' first", name)
	}

	imgName := cfg.ContainerImages[name]
	_, imgCfg, err := resolveImage(cfg, imgName)
	if err != nil {
		return discourseContainerContext{}, err
	}
	return discourseContainerContext{
		cfg:     cfg,
		name:    name,
		workdir: config.EffectiveWorkdir(cfg, imgCfg, name),
	}, nil
}

func init() {
	pluginCmd.PersistentFlags().String("name", "", "Agent/container name (defaults to selected agent)")
	pluginAddCmd.Flags().String("branch", "", "Branch to checkout for all added plugins")
	pluginAddCmd.Flags().Bool("skip-maintenance", false, "Skip bundle install and database migrations after cloning")
	pluginCmd.AddCommand(pluginAddCmd)
	pluginCmd.AddCommand(pluginListCmd)
}
