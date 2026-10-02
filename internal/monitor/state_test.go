package monitor

import (
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// run feeds a sequence of statuses (u=up g=degraded d=down) and returns the
// state after each step plus the transitions that fired.
func run(m *Machine, seq string) (states string, transitions []Transition) {
	code := map[byte]check.Status{'u': check.Up, 'g': check.Degraded, 'd': check.Down}
	short := map[State]byte{Unknown: '?', Up: 'u', Degraded: 'g', Down: 'd'}
	for i := 0; i < len(seq); i++ {
		if tr := m.Observe(check.Result{Status: code[seq[i]], At: t0.Add(time.Duration(i) * time.Minute)}); tr != nil {
			transitions = append(transitions, *tr)
		}
		states += string(short[m.State])
	}
	return
}

func TestMachine(t *testing.T) {
	cases := []struct {
		name       string
		start      State
		fail, rec  int
		seq        string
		wantStates string
		wantTrans  int
	}{
		{"unknown to up is immediate", Unknown, 3, 2, "u", "u", 1},
		{"unknown to down needs fail threshold", Unknown, 3, 2, "ddd", "??d", 1},
		{"single blip is ignored", Up, 3, 2, "uduuu", "uuuuu", 0},
		{"two blips then recover is ignored", Up, 3, 2, "ddudd", "uuuuu", 0},
		{"three consecutive failures go down", Up, 3, 2, "ddd", "uud", 1},
		{"recovery needs recover threshold", Down, 3, 2, "udu" + "u", "dddu", 1},
		{"flapping never alerts", Up, 3, 2, "dudududu", "uuuuuuuu", 0},
		{"mixed degraded/down streak counts as failing", Up, 3, 2, "gdd", "uud", 1},
		{"ends on latest status", Up, 3, 2, "ddg", "uug", 1},
		{"down to degraded is an improvement", Down, 3, 2, "gg", "dg", 1},
		{"degraded to down is worsening", Degraded, 2, 2, "dd", "gd", 1},
		{"full outage cycle", Up, 2, 2, "uddduuu", "uuddduu", 2},
		{"threshold 1 is immediate", Up, 1, 1, "du", "du", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewMachine(c.start, t0, c.fail, c.rec)
			states, trs := run(m, c.seq)
			if states != c.wantStates {
				t.Errorf("states = %q, want %q", states, c.wantStates)
			}
			if len(trs) != c.wantTrans {
				t.Errorf("transitions = %d, want %d (%+v)", len(trs), c.wantTrans, trs)
			}
		})
	}
}

func TestTransitionCarriesTimeOfConfirmingResult(t *testing.T) {
	m := NewMachine(Up, t0, 3, 2)
	_, trs := run(m, "ddd")
	if want := t0.Add(2 * time.Minute); !trs[0].At.Equal(want) || !m.Since.Equal(want) {
		t.Fatalf("At=%v Since=%v, want %v", trs[0].At, m.Since, want)
	}
}

func TestPausedRestoresAsUnknown(t *testing.T) {
	if m := NewMachine(Paused, t0, 3, 2); m.State != Unknown {
		t.Fatalf("got %s", m.State)
	}
}
