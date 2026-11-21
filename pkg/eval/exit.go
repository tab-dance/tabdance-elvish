package eval

import (
	"errors"
	"strconv"
)

// Exit is a special error used to request that the Elvish process exit.
// Unlike ordinary exceptions, it is not caught by try/catch and propagates
// through closure boundaries, running defers along the way.
type Exit struct {
	Code int
}

func (e Exit) Error() string { return "exit " + strconv.Itoa(e.Code) }

// AsExit returns the exit code and true if err is or wraps an Exit.
func AsExit(err error) (int, bool) {
	var e Exit
	if errors.As(err, &e) {
		return e.Code, true
	}
	var exc Exception
	if errors.As(err, &exc) {
		if e, ok := Reason(exc).(Exit); ok {
			return e.Code, true
		}
	}
	return 0, false
}
