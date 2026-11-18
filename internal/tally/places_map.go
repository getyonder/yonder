//go:build unix

package tally

import (
	"errors"
	"os"
	"syscall"
)

// mapFile gives the bytes of a file without reading them: the kernel maps
// the file, and brings in only the pages that are looked at. release ends
// the mapping; the bytes must not be used after it. The file may be replaced
// by another of the same name meanwhile (not written over): the mapping
// keeps the old one.
func mapFile(file string) (data []byte, release func(), err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Size() == 0 || st.Size() != int64(int(st.Size())) {
		return nil, nil, errors.New("it is empty or too large")
	}
	data, err = syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() { syscall.Munmap(data) }, nil
}
