package resources

import "testing"

func TestEmbeddedInstructionVariablesArePopulated(t *testing.T) {
	if DefaultInstructions == "" {
		t.Fatal("DefaultInstructions should be embedded")
	}
	if SimpleInstructions == "" {
		t.Fatal("SimpleInstructions should be embedded")
	}
	if NsfwInstructions == "" {
		t.Fatal("NsfwInstructions should be embedded")
	}
	if CCInstructions == "" {
		t.Fatal("CCInstructions should be embedded")
	}
	if Instructions != NsfwInstructions {
		t.Fatal("Instructions should default to NsfwInstructions")
	}
}
