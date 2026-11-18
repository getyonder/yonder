//go:build !unix

package tally

import "os"

// mapFile, where a file cannot be mapped, reads it. Yonder's servers run
// Linux; this is here so that the package builds everywhere.
func mapFile(file string) (data []byte, release func(), err error) {
	data, err = os.ReadFile(file)
	return data, func() {}, err
}
