package trace

// Costs returns the cost records of every workstream in one scan. It fails on
// any damaged record rather than report a partial total.
func (r *Repository) Costs() ([]Cost, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, _, err := r.scan()
	if err != nil {
		return nil, err
	}
	var out []Cost
	for _, v := range records {
		if c, ok := v.(Cost); ok {
			out = append(out, c)
		}
	}
	return out, nil
}
