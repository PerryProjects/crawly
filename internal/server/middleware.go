package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/observability"
)

// Context keys set by handlers and read by the access log.
const (
	ctxHost  = "siteinfo.host"
	ctxCache = "siteinfo.cache"
)

// requestID adopts a sane incoming X-Request-Id or generates one, and echoes it.
func (s *Server) requestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := c.Request().Header.Get(echo.HeaderXRequestID)
		if !validRequestID(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		c.Response().Header().Set(echo.HeaderXRequestID, id)

		ctx := observability.WithLogger(c.Request().Context(), s.log.With(zap.String("request_id", id)))
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// accessLog writes the access log and request metrics. It renders errors itself
// so it can see the final status. It never logs the query string or headers
// (which carry the URL under inspection and the bearer token).
func (s *Server) accessLog(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		start := time.Now()
		if err := next(c); err != nil {
			c.Echo().HTTPErrorHandler(c, err)
		}

		res, _ := echo.UnwrapResponse(c.Response())
		status := http.StatusOK
		var size int64
		if res != nil {
			status, size = res.Status, res.Size
		}
		route := c.Path()
		if route == "" {
			route = "unmatched" // bounded label cardinality
		}
		d := time.Since(start)
		s.obs.ObserveRequest(route, c.Request().Method, status, d)

		log := observability.LoggerFrom(c.Request().Context(), s.log)
		fields := []zap.Field{
			zap.String("method", c.Request().Method), zap.String("route", route), zap.Int("status", status),
			zap.Duration("duration", d), zap.Int64("bytes", size), zap.String("remote_ip", c.RealIP()),
		}
		if v, ok := c.Get(ctxHost).(string); ok {
			fields = append(fields, zap.String("host", v))
		}
		if v, ok := c.Get(ctxCache).(string); ok {
			fields = append(fields, zap.String("cache", v))
		}
		switch {
		case route == "/healthz" || route == "/readyz":
			log.Debug("request", fields...)
		case status >= 500:
			log.Error("request", fields...)
		case status >= 400:
			log.Warn("request", fields...)
		default:
			log.Info("request", fields...)
		}
		return nil // the error, if any, has been rendered above
	}
}

// recoverPanic turns a handler panic into a 500 through the normal error path.
func (s *Server) recoverPanic(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				observability.LoggerFrom(c.Request().Context(), s.log).Error("panic in handler",
					zap.Any("panic", r), zap.ByteString("stack", debug.Stack()))
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return next(c)
	}
}

var errUnauthorized = errors.New("unauthorized")

// authenticate enforces a static bearer token, compared in constant time.
func (s *Server) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if s.noAuth {
			return next(c)
		}
		got, _ := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		match := 0
		for _, t := range s.tokenSums { // no early exit: time does not depend on which token matched
			match |= subtle.ConstantTimeCompare(sum[:], t[:])
		}
		if match != 1 {
			c.Response().Header().Set("WWW-Authenticate", "Bearer")
			return errUnauthorized
		}
		return next(c)
	}
}
