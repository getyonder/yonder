//go:build !windows

package control

import (
	"os"
	"syscall"
)

// ownedByMe reports whether the file is owned by the user running this.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
