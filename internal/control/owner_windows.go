package control

import "os"

// ownedByMe is always true here: the socket's folder is under the user's own
// local application data on Windows, and a folder's owner is not read.
func ownedByMe(os.FileInfo) bool { return true }
