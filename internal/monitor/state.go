// Package monitor holds the state machine that turns noisy probe results into
// trustworthy state changes. It is pure (no I/O, no clock) so it can be tested
// exhaustively.
//
// Rules:
//   - A state only changes after a streak of consecutive results that differ
//     from it. One blip never flips a monitor.
//   - Getting worse needs FailThreshold results; getting better needs
//     RecoverThreshold results. A streak containing anything worse than the
//     current state is treated as "getting worse".
//   - When a streak qualifies, the monitor moves to the LATEST result's status.
//   - Any result equal to the current state resets the streak.
package monitor

import (
	"time"

	"github.com/mali-harsh/vigil/internal/check"
)

type State string

const (
	Unknown  State = "unknown"
	Up       State = "up"
	Degraded State = "degraded"
	Down     State = "down"
	Paused   State = "paused"
)

func rank(s State) int {
	switch s {
	case Up:
		return 0
	case Degraded:
		return 1
	case Down:
		return 2
	}
	return -1
}

type Transition struct {
	From, To State
	At       time.Time
	Lasted   time.Duration // how long the previous state held
	Reason   string
}

type Machine struct {
	State            State
	Since            time.Time
	FailThreshold    int
	RecoverThreshold int

	streak int
	worst  State
}

func NewMachine(state State, since time.Time, fail, recover int) *Machine {
	if state == "" || state == Paused {
		state = Unknown
	}
	return &Machine{State: state, Since: since, FailThreshold: max(fail, 1), RecoverThreshold: max(recover, 1)}
}

// Observe feeds one result in and returns a Transition if the state changed.
func (m *Machine) Observe(r check.Result) *Transition {
	got := State(r.Status)
	if got == m.State {
		m.streak, m.worst = 0, ""
		return nil
	}
	m.streak++
	if m.worst == "" || rank(got) > rank(m.worst) {
		m.worst = got
	}

	need := m.RecoverThreshold
	switch {
	case m.State == Unknown && got != Down:
		need = 1 // first sight of a healthy target: no reason to wait
	case rank(m.worst) > rank(m.State):
		need = m.FailThreshold
	}
	if m.streak < need {
		return nil
	}

	t := &Transition{From: m.State, To: got, At: r.At, Lasted: r.At.Sub(m.Since), Reason: r.Message}
	m.State, m.Since = got, r.At
	m.streak, m.worst = 0, ""
	return t
}

// Pending reports how many differing results are in the current streak —
// useful in the UI ("2/3 failures, confirming...").
func (m *Machine) Pending() int { return m.streak }
