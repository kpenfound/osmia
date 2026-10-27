package trace

// CharterRepository returns the project's charter authority.
func (r *Repository) CharterRepository() (*Repository, error) {
	if r.parent != nil {
		return r.parent.CharterRepository()
	}
	return r, nil
}

// ProjectDocuments reads the project's document history.
func (r *Repository) ProjectDocuments() ([]Document, error) { return Read[Document](r, "") }
