package session

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound           = errors.New("session: not found")
	ErrSessionLimit       = errors.New("session: active session limit reached")
	ErrViewerLimit        = errors.New("session: viewer limit reached")
	ErrInvalidState       = errors.New("session: invalid state")
	ErrInvalidArgument    = errors.New("session: invalid argument")
	ErrReplayGap          = errors.New("session: replay unavailable")
	ErrSlowConsumer       = errors.New("session: slow consumer")
	ErrProcessFailed      = errors.New("session: process failed")
	ErrClosed             = errors.New("session: manager closed")
	ErrSubscriptionClosed = errors.New("session: subscription closed")
	ErrInputRevoked       = errors.New("session: input no longer authorized")
	ErrCommandDenied      = errors.New("session: command not permitted")
)

// ReplayGapError reports that output after AfterSeq is no longer buffered.
// FirstSeq is the oldest buffered sequence and LastSeq the newest.
type ReplayGapError struct {
	AfterSeq, FirstSeq, LastSeq uint64
}

func (e *ReplayGapError) Error() string {
	return fmt.Sprintf("session: replay unavailable after seq %d (buffered %d..%d)", e.AfterSeq, e.FirstSeq, e.LastSeq)
}

func (e *ReplayGapError) Is(target error) bool { return target == ErrReplayGap }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidArgument}, args...)...)
}
