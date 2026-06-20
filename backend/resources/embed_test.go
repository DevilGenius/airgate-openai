package resources

import "testing"

func TestResolveInstructions(t *testing.T) {
	cases := []struct {
		name string
		arg  string
		want string
	}{
		{name: "default", arg: "default", want: DefaultInstructions},
		{name: "simple", arg: "simple", want: SimpleInstructions},
		{name: "nsfw", arg: "nsfw", want: NsfwInstructions},
		{name: "cc", arg: "cc", want: CCInstructions},
		{name: "custom text", arg: "custom instructions", want: "custom instructions"},
		{name: "case sensitive alias", arg: "DEFAULT", want: "DEFAULT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveInstructions(tc.arg); got != tc.want {
				t.Fatalf("ResolveInstructions(%q) mismatch", tc.arg)
			}
		})
	}
}

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
