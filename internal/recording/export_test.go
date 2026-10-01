package recording

import "errors"

// FailEncoding makes the service fail to encode events of kind.
func FailEncoding(s *Service, kind Kind) {
	s.encode = func(e Event) ([]byte, error) {
		if e.Kind == kind {
			return nil, errors.New("recording: injected encoding failure")
		}
		return encodeEvent(e)
	}
}
