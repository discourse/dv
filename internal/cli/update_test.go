package cli

import "testing"

func TestResolveAgentUpdateStepsTargetsSingleAgent(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps("term-llm")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "term-llm" {
		t.Fatalf("name = %q, want %q", name, "term-llm")
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	if steps[0].label != "Term-LLM" {
		t.Fatalf("step label = %q, want %q", steps[0].label, "Term-LLM")
	}
}

func TestResolveAgentUpdateStepsSupportsAlias(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps("tl")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "term-llm" {
		t.Fatalf("name = %q, want %q", name, "term-llm")
	}
	if len(steps) != 1 || steps[0].name != "term-llm" {
		t.Fatalf("steps = %#v, want only term-llm", steps)
	}
}

func TestResolveAgentUpdateStepsEmptyReturnsAllAgents(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps("")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "" {
		t.Fatalf("name = %q, want empty", name)
	}
	if len(steps) != len(agentUpdateSteps) {
		t.Fatalf("len(steps) = %d, want %d", len(steps), len(agentUpdateSteps))
	}
}

func TestResolveAgentUpdateStepsRejectsUnknownAgent(t *testing.T) {
	if _, _, err := resolveAgentUpdateSteps("not-an-agent"); err == nil {
		t.Fatal("resolveAgentUpdateSteps() error = nil, want error")
	}
}
