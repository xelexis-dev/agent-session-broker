// Package memory persists immutable Markdown nodes in a project-local graph.
//
// Memory storage supports Windows, Linux and macOS, excluding Android.
// NewStore fails with ErrInvalidInput on other platforms rather than using
// mutable pathnames. Supported stores require hard-link support because
// completed nodes are published with an atomic no-replace link operation.
// Linux also requires procfs access through /proc/self/fd. NewStore fails
// safely if that pinned-directory path is unavailable. macOS uses
// descriptor-relative operations instead of procfs. Stores retain the pinned
// nodes directory for their lifetime so concurrent path replacement cannot redirect
// reads or writes. Call Store.Close to release that platform resource.
package memory
