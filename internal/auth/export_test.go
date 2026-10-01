package auth

// SetLoginVerifiedHook runs hook after Login verifies the password and before
// it persists a session.
func SetLoginVerifiedHook(s *Service, hook func()) { s.hooks.loginVerified = hook }

// SetPasswordChangeVerifyHook runs hook immediately before ChangePassword
// verifies the current password.
func SetPasswordChangeVerifyHook(s *Service, hook func()) { s.hooks.passwordChangeVerify = hook }

// SetPasswordChangeVerifiedHook runs hook after ChangePassword accepts the
// current password and before it commits the rotation.
func SetPasswordChangeVerifiedHook(s *Service, hook func()) { s.hooks.passwordChangeVerified = hook }
