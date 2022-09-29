package log

import (
	"net/http"
	"os"
	"sync/atomic"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var logger atomic.Value

func load() *zerolog.Logger {
	if l := logger.Load(); l == nil {
		consoleLogger := zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		logger.Store(&consoleLogger)
	}
	return logger.Load().(*zerolog.Logger)
}

type HttpLogger struct {
	http.ResponseWriter
	status int
}

func Http(rw http.ResponseWriter) *HttpLogger {
	return &HttpLogger{ResponseWriter: rw}
}

func (l *HttpLogger) WriteHeader(statusCode int) {
	l.ResponseWriter.WriteHeader(statusCode)
	l.status = statusCode
}

func (l HttpLogger) Log() *zerolog.Event {
	if l.status < 400 {
		return load().Info().Int("status", l.status)
	}
	return load().Error().Int("status", l.status)
}
