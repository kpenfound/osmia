package intake

// apiErrAsError lets readview.Consistent's fn, whose signature returns the
// standard error interface, carry an *APIError through a retry so the
// caller can recover it with errors.As.
type ApiErrAsError struct{ Api *APIError }

func (e ApiErrAsError) Error() string { return e.Api.Message }
