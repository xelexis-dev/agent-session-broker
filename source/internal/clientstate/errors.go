package clientstate

import "errors"

// ErrLocked means another local client process already owns this registration.
var ErrLocked = errors.New("another local MCP client already holds this registration; stop it before registering again")
