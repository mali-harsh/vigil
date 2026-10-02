package engine

import "time"

// SetTimings shortens agent timeout and housekeeping for tests.
// Must be called before Run.
func (e *Engine) SetTimings(agentTimeout, tick time.Duration) {
	e.agentTimeout, e.tickEvery, e.remindEvery = agentTimeout, tick, tick
	for _, a := range e.agents {
		a.lastSeen = e.now()
	}
}

var Derive = derive
