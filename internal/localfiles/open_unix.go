//go:build unix

package localfiles

import (
	"os"
	"syscall"
)

func openRegular(r *os.Root, p string) (*os.File, error) {
	st, err := r.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil, ErrAccess
	}
	return r.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
