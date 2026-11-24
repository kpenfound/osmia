package errorreport

import "time"

func decideAndApply(st *State, cfg Config, in Input, now time.Time) Decision {
	d := Decide(st, cfg, in, now)
	st.Apply(d)
	return d
}
