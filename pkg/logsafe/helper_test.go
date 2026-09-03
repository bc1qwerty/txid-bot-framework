package logsafe

import (
	"io"
	stdlog "log"
)

func newTestLogger(w io.Writer) *stdlog.Logger { return stdlog.New(w, "", 0) }
