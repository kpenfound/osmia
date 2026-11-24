package issues

import "errors"

func IsUncertain(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Uncertain()
}

func IsRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Rejected()
}
