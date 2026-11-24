package errorreport

func (st *State) IntentsFor(repository string) []Intent {
	var out []Intent
	for _, in := range st.Intents {
		if sameRepo(in.Repository, repository) {
			out = append(out, in.clone())
		}
	}
	return out
}

func (st *State) AwaitingFor(repository string) []Awaiting {
	var out []Awaiting
	for _, aw := range st.Awaiting {
		if sameRepo(aw.Repository, repository) {
			out = append(out, aw)
		}
	}
	return out
}
