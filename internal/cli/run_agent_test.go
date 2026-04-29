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

func TestBuildAgentInteractiveTermLLMUsesChat(t *testing.T) {
	args := buildAgentInteractive("term-llm")

	want := []string{"term-llm", "chat", "@developer", "--yolo"}
	if len(args) != len(want) {
		t.Fatalf("expected %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, args)
		}
	}
}

func TestBuildAgentArgsTermLLMPutsDeveloperAndYoloAfterAsk(t *testing.T) {
	args := buildAgentArgs("term-llm", "hello")

	want := []string{"term-llm", "ask", "@developer", "--yolo", "hello"}
	if len(args) != len(want) {
		t.Fatalf("expected %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, args)
		}
	}
}
