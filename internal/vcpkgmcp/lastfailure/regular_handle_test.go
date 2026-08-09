package lastfailure

import (
	"io"
	"os"
)

type testRegularHandle struct {
	io.ReadCloser
	info os.FileInfo
}

func (f *testRegularHandle) Stat() (os.FileInfo, error) { return f.info, nil }
