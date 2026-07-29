package cli

import (
	"errors"
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

// errHelpShownForMissingArgs signals that a command was invoked without its
// required arguments and help was already printed; the error itself should not
// be shown to the user.
var errHelpShownForMissingArgs = errors.New("help shown for missing arguments")

func Execute() error {
	// Applied here rather than in init() so commands registered by other
	// files' init functions (e.g. upgrade) are covered too.
	showHelpOnMissingArgs(rootCmd)
	err := rootCmd.Execute()
	if errors.Is(err, errHelpShownForMissingArgs) {
		os.Exit(1)
	}
	return err
}

// showHelpOnMissingArgs wraps every command's positional-argument validator so
// that running a command that requires arguments with none at all prints that
// command's help instead of an error like "accepts 2 arg(s), received 0".
// Validation errors with some (wrong number of) arguments are unchanged.
func showHelpOnMissingArgs(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		showHelpOnMissingArgs(c)
		orig := c.Args
		if orig == nil {
			continue
		}
		c.Args = func(cc *cobra.Command, args []string) error {
			err := orig(cc, args)
			if err != nil && len(args) == 0 {
				_ = cc.Help()
				return errHelpShownForMissingArgs
			}
			return err
		}
	}
}

func addPersistentFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")
}

func init() {
	addPersistentFlags(rootCmd)

	// Custom usage template that keeps the command list aligned by padding only the
	// primary command name; aliases are shown after the description to avoid
	// breaking column alignment.
	rootCmd.SetUsageTemplate(`Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:
{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding}} {{.Short}}{{if gt (len .Aliases) 0}} (aliases: {{.Aliases}}){{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:
{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`)

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
