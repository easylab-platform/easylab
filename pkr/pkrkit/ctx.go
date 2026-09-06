package pkrkit

import "context"

// Ctx returns a background context for synchronous store helpers that don't
// have a request context handy.
func Ctx() context.Context { return context.Background() }
