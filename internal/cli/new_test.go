package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestConfirmInvalidRailsHostname(t *testing.T) {
	t.Run("warns and aborts by default", func(t *testing.T) {
		t.Parallel()

		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("\n"))
		errBuf := &bytes.Buffer{}
		cmd.SetErr(errBuf)

		proceed, err := confirmInvalidRailsHostname(cmd, "some_thing")
		if err != nil {
			t.Fatalf("confirmInvalidRailsHostname() error = %v", err)
		}
		if proceed {
			t.Fatal("confirmInvalidRailsHostname() = true, want false")
		}

		output := errBuf.String()
		if !strings.Contains(output, `Warning: agent name "some_thing" is not Rails hostname-safe.`) {
			t.Fatalf("warning output missing agent-name warning: %q", output)
		}
		if !strings.Contains(output, "numbers, letters, dashes, and dots") {
			t.Fatalf("warning output missing Rails hostname guidance: %q", output)
		}
		if !strings.Contains(output, "Continue anyway? (y/N): ") {
			t.Fatalf("warning output missing confirmation prompt: %q", output)
		}
	})

	t.Run("allows confirmed invalid name", func(t *testing.T) {
		t.Parallel()

		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("y\n"))
		errBuf := &bytes.Buffer{}
		cmd.SetErr(errBuf)

		proceed, err := confirmInvalidRailsHostname(cmd, "some_thing")
		if err != nil {
			t.Fatalf("confirmInvalidRailsHostname() error = %v", err)
		}
		if !proceed {
			t.Fatal("confirmInvalidRailsHostname() = false, want true")
		}
	})

	t.Run("skips warning for valid hostname", func(t *testing.T) {
		t.Parallel()

		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(""))
		errBuf := &bytes.Buffer{}
		cmd.SetErr(errBuf)

		proceed, err := confirmInvalidRailsHostname(cmd, "some-thing.1")
		if err != nil {
			t.Fatalf("confirmInvalidRailsHostname() error = %v", err)
		}
		if !proceed {
			t.Fatal("confirmInvalidRailsHostname() = false, want true")
		}
		if errBuf.Len() != 0 {
			t.Fatalf("confirmInvalidRailsHostname() wrote unexpected output: %q", errBuf.String())
		}
	})
}
