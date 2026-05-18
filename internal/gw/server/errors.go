package server

import "fmt"

// BadRequestError wraps a client-caused pipeline error (e.g. malformed JSON,
// invalid wire content) so HandleForward can distinguish it from provider and
// infrastructure failures and emit HTTP 400 instead of 500.
type BadRequestError struct {
	Message string
	Cause   error
}

func (e *BadRequestError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("bad request: %s: %v", e.Message, e.Cause)
	}
	return fmt.Sprintf("bad request: %s", e.Message)
}

func (e *BadRequestError) Unwrap() error {
	return e.Cause
}
