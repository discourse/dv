package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "dv",
	Short:         "Discourse Vibe: manage local Discourse dev containers",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
			os.Setenv("DV_VERBOSE", "1")
		}
	},
	CompletionOptions: cobra.CompletionOptions{
		DisableDefaultCmd: true,
	},
}

func Execute() error {
	return rootCmd.Execute()
}

func addPersistentFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")
}

// Command group IDs used to organize root help output.
const (
	groupDaily     = "daily"
	groupLifecycle = "lifecycle"
	groupCode      = "code"
	groupTools     = "tools"
)

func init() {
	addPersistentFlags(rootCmd)

	// Custom usage template that keeps the command list aligned by padding only the
	// primary command name; aliases are shown after the description to avoid
	// breaking column alignment. Commands render under their registered groups;
	// any ungrouped command falls back to "Additional Commands".
	rootCmd.SetUsageTemplate(`Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:
{{range $cmds}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding}} {{.Short}}{{if gt (len .Aliases) 0}} (aliases: {{.Aliases}}){{end}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{$group.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) .IsAvailableCommand)}}
  {{rpad .Name .NamePadding}} {{.Short}}{{if gt (len .Aliases) 0}} (aliases: {{.Aliases}}){{end}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") .IsAvailableCommand)}}
  {{rpad .Name .NamePadding}} {{.Short}}{{if gt (len .Aliases) 0}} (aliases: {{.Aliases}}){{end}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:
{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`)

	rootCmd.AddGroup(
		&cobra.Group{ID: groupDaily, Title: "Daily Use:"},
		&cobra.Group{ID: groupLifecycle, Title: "Container Lifecycle:"},
		&cobra.Group{ID: groupCode, Title: "Code & Plugins:"},
		&cobra.Group{ID: groupTools, Title: "Tools & Maintenance:"},
	)
	// The auto-generated help command must belong to a group, otherwise the
	// template renders an empty "Additional Commands" heading for it.
	rootCmd.SetHelpCommandGroupID(groupTools)

	// branchCmd, prCmd, and upgradeCmd are registered in their own files but
	// grouped here so the whole layout is visible in one place.
	groups := map[string][]*cobra.Command{
		groupDaily:     {enterCmd, restartCmd, resetCmd, runCmd, runAgentCmd, tuiCmd, branchCmd, prCmd, catchupCmd, copyCmd},
		groupLifecycle: {newCmd, startCmd, stopCmd, removeCmd, listCmd, selectCmd, renameCmd, psCmd},
		groupCode:      {importCmd, extractCmd, pluginCmd},
		groupTools:     {buildCmd, pullCmd, imageCmd, exposeCmd, mailCmd, serveCmd, configCmd, dataCmd, updateCmd, upgradeCmd, versionCmd},
	}
	for groupID, cmds := range groups {
		for _, c := range cmds {
			c.GroupID = groupID
		}
	}

	rootCmd.AddCommand(buildCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(enterCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(runAgentCmd)
	rootCmd.AddCommand(copyCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(removeCmd)
	rootCmd.AddCommand(exposeCmd)
	rootCmd.AddCommand(mailCmd)
	rootCmd.AddCommand(tuiCmd)
	// Top-level agent management commands
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(newCmd)
	rootCmd.AddCommand(pluginCmd)
	rootCmd.AddCommand(selectCmd)
	rootCmd.AddCommand(renameCmd)
	rootCmd.AddCommand(extractCmd)
	rootCmd.AddCommand(importCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(dataCmd)
	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(catchupCmd)
	rootCmd.AddCommand(pullCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(serveCmd)

	setupUpdateChecks()
	setupUpgradeCommand()
}

func exitIfErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
