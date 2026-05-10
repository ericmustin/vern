package cmd

import "testing"

func TestDefaultAgentSkillPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default workflows name", in: "workflows.yaml", want: "agent-skill.md"},
		{name: "singular workflow name", in: "workflow.yml", want: "agent-skill.md"},
		{name: "custom output name", in: "build/review.yaml", want: "build/review-agent-skill.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultAgentSkillPath(tt.in); got != tt.want {
				t.Fatalf("defaultAgentSkillPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
