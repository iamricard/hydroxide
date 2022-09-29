package log

import (
	"os"
	"sync/atomic"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var logger atomic.Value

func L() *zerolog.Logger {
	if l := logger.Load(); l == nil {
		consoleLogger := zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		logger.Store(&consoleLogger)
	}
	return logger.Load().(*zerolog.Logger)
}
