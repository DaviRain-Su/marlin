package client

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrKind is the client-facing classification of a gRPC failure. The UI sees
// only this enum; the mapping lives in one place (design §6.7).
type ErrKind int

const (
	// ErrUnknown covers codes with no dedicated mapping; retry is likely safe.
	ErrUnknown ErrKind = iota
	// ErrHostUnavailable: connection refused, host down (UNAVAILABLE).
	ErrHostUnavailable
	// ErrCorruptStream: incomplete/malformed stream (DATA_LOSS).
	ErrCorruptStream
	// ErrSubmissionConflict: same submission_id with different input
	// (ALREADY_EXISTS). Keep the draft, mint a new ID.
	ErrSubmissionConflict
	// ErrConcurrentAdmission: lost an admission race (ABORTED). Retry the
	// exact request.
	ErrConcurrentAdmission
	// ErrPrecondition: active/unresolved turn or stale after_turn
	// (FAILED_PRECONDITION). Calibrate via GetSessionSummary.
	ErrPrecondition
	// ErrLimitExceeded: input budget refused. Keep the draft, show the limit.
	ErrLimitExceeded
	// ErrCanceled: caller context canceled.
	ErrCanceled
)

// Error is a classified host error.
type Error struct {
	Kind    ErrKind
	Code    codes.Code
	Method  string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("marlin %s: %s: %s", e.Method, e.Code, e.Message)
}

// AsErrKind extracts the classification from err, if any.
func AsErrKind(err error) (ErrKind, bool) {
	var e *Error
	if ok := as(err, &e); ok {
		return e.Kind, true
	}
	return ErrUnknown, false
}

// as is errors.As without importing errors here for a single use.
func as(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func classify(err error, method string) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	kind := ErrUnknown
	switch st.Code() {
	case codes.Unavailable:
		kind = ErrHostUnavailable
	case codes.DataLoss:
		kind = ErrCorruptStream
	case codes.AlreadyExists:
		kind = ErrSubmissionConflict
	case codes.Aborted:
		kind = ErrConcurrentAdmission
	case codes.FailedPrecondition:
		kind = ErrPrecondition
	case codes.ResourceExhausted:
		kind = ErrLimitExceeded
	case codes.Canceled:
		kind = ErrCanceled
	}
	return &Error{Kind: kind, Code: st.Code(), Method: method, Message: st.Message()}
}
