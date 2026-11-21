package agent

import "testing"

// TestUtilityTemperature pins the judge's side of "temperature 0": a definition
// that sets judge_temperature gets exactly that value, as a pointer, so the
// provider sends 0 instead of omitting the field. Everything else keeps its
// model-derived value, and a malformed setting degrades to it rather than
// failing the call.
func TestUtilityTemperature(t *testing.T) {
	pinned := float32(0.4)
	spec := ResolvedAgent{Settings: map[string]string{"judge_temperature": "0"}}
	got := utilityTemperature(spec)
	if got == nil || *got != 0 {
		t.Fatalf("judge_temperature=0 gave %v, want a pointer to 0", got)
	}

	// No setting: the model entry's own temperature applies untouched.
	spec = ResolvedAgent{Temperature: &pinned}
	if got := utilityTemperature(spec); got != &pinned {
		t.Fatalf("without the setting the model value must survive, got %v", got)
	}
	spec = ResolvedAgent{}
	if got := utilityTemperature(spec); got != nil {
		t.Fatalf("no setting and no model value must stay nil, got %v", got)
	}

	// An unparseable setting is ignored, not fatal.
	spec = ResolvedAgent{Temperature: &pinned, Settings: map[string]string{"judge_temperature": "cold"}}
	if got := utilityTemperature(spec); got != &pinned {
		t.Fatalf("a bad setting must fall back to the model value, got %v", got)
	}
}
