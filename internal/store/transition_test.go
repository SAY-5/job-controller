package store

import "testing"

func TestIsValidTransitionTable(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StatePending, StateRunning, true},
		{StatePending, StateCompleted, false},
		{StateRunning, StateCheckpointing, true},
		{StateRunning, StateCompleted, true},
		{StateCheckpointing, StateRunning, true},
		{StateCompleted, StateRunning, false},
		{StateCompleted, StateCompleted, true},
		{StateInterruptedResumable, StateRunning, true},
		{StateInterruptedResumable, StateCheckpointing, false},
		{StateInterruptedUnresumable, StateRunning, false},
	}
	for _, c := range cases {
		got := IsValidTransition(c.from, c.to)
		if got != c.want {
			t.Errorf("transition %s -> %s = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestStateIsTerminal(t *testing.T) {
	cases := map[State]bool{
		StatePending:                false,
		StateRunning:                false,
		StateCheckpointing:          false,
		StateInterruptedResumable:   false,
		StateInterruptedUnresumable: true,
		StateCompleted:              true,
		StateFailed:                 true,
		StateCancelled:              true,
	}
	for s, want := range cases {
		if s.IsTerminal() != want {
			t.Errorf("%s.IsTerminal() = %v want %v", s, s.IsTerminal(), want)
		}
	}
}
