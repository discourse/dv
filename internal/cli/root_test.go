package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestShowHelpOnMissingArgs(t *testing.T) {
	root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
	ran := false
	child := &cobra.Command{
		Use:  "add NAME",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ran = true
		},
	}
	root.AddCommand(child)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"add"})

	showHelpOnMissingArgs(root)
	err := root.Execute()

	if !errors.Is(err, errHelpShownForMissingArgs) {
		t.Fatalf("expected silent missing-argument error, got %v", err)
	}
	if ran {
		t.Fatal("command ran despite missing required argument")
	}
	if stdout.Len() != 0 {
		t.Fatalf("missing-argument help must not be written to stdout:\n%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "Usage:\n  test add NAME") {
		t.Fatalf("expected command help on stderr, got:\n%s", got)
	}
}

func TestIsSilentError(t *testing.T) {
	if !IsSilentError(fmt.Errorf("wrapped: %w", errHelpShownForMissingArgs)) {
		t.Fatal("expected wrapped missing-argument error to be silent")
	}
	if IsSilentError(errors.New("ordinary error")) {
		t.Fatal("ordinary error must not be silent")
	}
}

func TestShowHelpOnMissingArgsLeavesOtherArityErrorsUnchanged(t *testing.T) {
	root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
	child := &cobra.Command{Use: "rename OLD NEW", Args: cobra.ExactArgs(2), Run: func(cmd *cobra.Command, args []string) {}}
	root.AddCommand(child)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"rename", "old"})

	showHelpOnMissingArgs(root)
	err := root.Execute()

	if err == nil || errors.Is(err, errHelpShownForMissingArgs) {
		t.Fatalf("expected original arity error, got %v", err)
	}
	if !strings.Contains(err.Error(), "accepts 2 arg(s), received 1") {
		t.Fatalf("unexpected error: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("help should not be shown for a non-empty argument list:\n%s", output.String())
	}
}

func TestShowHelpOnMissingArgsIsIdempotent(t *testing.T) {
	root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
	child := &cobra.Command{Use: "add NAME", Args: cobra.ExactArgs(1), Run: func(cmd *cobra.Command, args []string) {}}
	root.AddCommand(child)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"add"})

	showHelpOnMissingArgs(root)
	showHelpOnMissingArgs(root)
	_ = root.Execute()

	if count := strings.Count(output.String(), "Usage:"); count != 1 {
		t.Fatalf("expected help once after repeated decoration, got %d:\n%s", count, output.String())
	}
}
