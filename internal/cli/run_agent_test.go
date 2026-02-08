package cli

import "testing"

func TestBuildAgentArgsCodexIncludesSearchBeforeExec(t *testing.T) {
	args := buildAgentArgs("codex", "abc")

	if len(args) == 0 || args[0] != "codex" {
		t.Fatalf("unexpected argv: %v", args)
	}

	searchIdx := -1
	execIdx := -1
	for i, arg := range args {
		switch arg {
		case "--search":
			searchIdx = i
		case "exec":
			execIdx = i
		}
	}

	if searchIdx == -1 {
		t.Fatalf("expected '--search' in args, got %v", args)
	}
	if execIdx == -1 {
		t.Fatalf("expected exec subcommand in args, got %v", args)
	}
	if searchIdx > execIdx {
		t.Fatalf("expected --search to appear before exec, got %v", args)
	}
}
