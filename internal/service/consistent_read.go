package service

// apiErrAsError lets readview.Consistent's fn, whose signature returns the
// standard error interface, carry an *APIError through a retry so the
// caller can recover it with errors.As.
type apiErrAsError struct{ api *APIError }

func (e apiErrAsError) Error() string { return e.api.Message }

// storeGeneration is the service's runtime store's own Generation, suitable
// as a consistentRead source. Several tests build a *Service directly with
// no store, the same way production code treats a nil store elsewhere
// (archive.go, inbox.go's loopEntry, loop_guard.go, notify.go, pause.go) as
// having no overrides, so this reports a generation that never changes
// rather than panicking on the nil receiver.
func (s *Service) storeGeneration() uint64 {
	if s.store == nil {
		return 0
	}
	return s.store.Generation()
}
