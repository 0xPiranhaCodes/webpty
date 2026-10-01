package access

import "context"

// SetBeforeCommit makes s run hook at the end of every mutating transaction.
func SetBeforeCommit(s *Service, hook func(context.Context) error) { s.beforeCommit = hook }
