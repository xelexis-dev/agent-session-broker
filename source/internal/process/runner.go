package process

import (
	"context"
)

type Command struct {
	Path string
	Args []string
	CWD  string
	Env  map[string]string
}
type Result struct {
	Stdout string
	Stderr string
	Err    error
}
type Process interface {
	PID() int
	Done() <-chan Result
	Activity() <-chan struct{}
	Stop() error
}
type Starter interface {
	Start(context.Context, Command) (Process, error)
}
