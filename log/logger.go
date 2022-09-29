package log

import (
	"errors"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var logger atomic.Value

func Load() *zerolog.Logger {
	if l := logger.Load(); l == nil {
		consoleLogger := zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		logger.Store(&consoleLogger)
	}
	return logger.Load().(*zerolog.Logger)
}

type HttpLogger struct {
	http.ResponseWriter
	status       int
	responseSize int
	err          error
}

var _ http.ResponseWriter = &HttpLogger{}

func Http(rw http.ResponseWriter) *HttpLogger {
	return &HttpLogger{ResponseWriter: rw}
}

func (l *HttpLogger) WriteHeader(statusCode int) {
	l.ResponseWriter.WriteHeader(statusCode)
	l.status = statusCode
}

func (l *HttpLogger) Write(b []byte) (int, error) {
	size, err := l.ResponseWriter.Write(b)
	l.responseSize = size
	if l.status > 400 {
		l.err = errors.New(string(b))
	}
	return size, err
}

func (l HttpLogger) Log() *zerolog.Event {
	return Load().
		Err(l.err).
		Int("status", l.status).
		Int("response_size", l.responseSize)
}
