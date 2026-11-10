//go:build darwin

package channel

import "github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"

func kimiProcessKey(pid int) (string, error) { v, e := processinfo.Read(pid); return v.Key, e }
func kimiParentPID(pid int) (int, error)     { return processinfo.Parent(pid) }
