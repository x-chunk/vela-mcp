//go:build !unix

package localfiles

import "os"

func openRegular(r *os.Root, p string) (*os.File, error) {
	st, err := r.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil, ErrAccess
	}
	return r.Open(p)
}
