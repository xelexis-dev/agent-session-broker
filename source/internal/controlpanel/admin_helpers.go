package controlpanel

import "errors"

// check runs only with Store.mu held; callbacks must not acquire it again.
func (g commitGuard) check() error {
	if g != nil {
		return g()
	}
	return nil
}

type userChange struct {
	Before, After            User
	PasswordSet, RaisesPower bool
}

var errProjectGenerationMismatch = errors.New("project generation mismatch")
