package server

import (
	"context"
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
	"go.uber.org/zap/zapcore"

	"github.com/OWNER/siteinfo/internal/observability"
)

// ctxCache is the echo context key under which handlers record the cache outcome for the access log.
const ctxCache = "siteinfo.cache"

// requestID adopts a sane incoming X-Request-Id or generates one, echoes it, and
// stores the request-scoped log fields in the request context.
func (s *Server) requestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := c.Request().Header.Get(echo.HeaderXRequestID)
		if !validRequestID(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		c.Response().Header().Set(echo.HeaderXRequestID, id)

		ctx := observability.WithRequestFields(c.Request().Context(), &observability.RequestFields{RequestID: id})
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

// accessEntry is one access log record. It implements zapcore.ObjectMarshaler
// so it is encoded field by field, without reflection and without allocating;
// instances are pooled for the same reason.
type accessEntry struct {
	Method   string
	Route    string
	RemoteIP string
	Cache    string
	Status   int
	Bytes    int64
	Duration time.Duration

	// fields backs the variadic argument of the log call. A variadic slice built at the
	// call site escapes to the heap (zap writes through an interface), so it lives in
	// the pooled entry instead.
	fields [2]zap.Field
}

// MarshalLogObject implements zapcore.ObjectMarshaler.
func (e *accessEntry) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("method", e.Method)
	enc.AddString("route", e.Route)
	enc.AddInt("status", e.Status)
	enc.AddDuration("duration", e.Duration)
	enc.AddInt64("bytes", e.Bytes)
	enc.AddString("remote_ip", e.RemoteIP)
	if e.Cache != "" {
		enc.AddString("cache", e.Cache)
	}
	return nil
}

// logAccess writes the access log line. Probes are logged at debug level.
func (s *Server) logAccess(ctx context.Context, e *accessEntry) {
	var level zapcore.Level
	switch {
	case e.Route == "/healthz" || e.Route == "/readyz":
		level = zapcore.DebugLevel
	case e.Status >= http.StatusInternalServerError:
		level = zapcore.ErrorLevel
	case e.Status >= http.StatusBadRequest:
		level = zapcore.WarnLevel
	default:
		level = zapcore.InfoLevel
	}
	if ce := s.log.Check(level, "request"); ce != nil {
		e.fields[0], e.fields[1] = observability.Fields(ctx), zap.Inline(e)
		ce.Write(e.fields[:]...)
	}
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

		e := s.entries.Get().(*accessEntry)
		e.Status, e.Bytes = http.StatusOK, 0
		if res, _ := echo.UnwrapResponse(c.Response()); res != nil {
			e.Status, e.Bytes = res.Status, res.Size
		}
		e.Route = c.Path()
		if e.Route == "" {
			e.Route = "unmatched" // bounded label cardinality
		}
		e.Method, e.RemoteIP, e.Duration = c.Request().Method, c.RealIP(), time.Since(start)
		e.Cache, _ = c.Get(ctxCache).(string)

		s.obs.ObserveRequest(e.Route, e.Method, e.Status, e.Duration)
		s.logAccess(c.Request().Context(), e)
		*e = accessEntry{}
		s.entries.Put(e)
		return nil // the error, if any, has been rendered above
	}
}

// recoverPanic turns a handler panic into a 500 through the normal error path.
func (s *Server) recoverPanic(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				s.sugar.Errorw("panic in handler", observability.Fields(c.Request().Context()),
					"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return next(c)
	}
}

// limitError is a protection rejection (rate limit or load shedding).
type limitError struct {
	status     int
	code       string
	retryAfter time.Duration
}

func (e *limitError) Error() string { return "rejected: " + e.code }

func (s *Server) reject(status int, code, reason string, wait time.Duration) error {
	s.obs.Rejected(reason)
	return &limitError{status: status, code: code, retryAfter: wait}
}

// protect applies the cheap, unauthenticated checks first:
//
//  1. per-IP token bucket (429)
//  2. in-flight cap: shed instead of queueing (503)
//
// Repeated authentication failures are handled in authenticate.
func (s *Server) protect(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ip := clientKey(c.RealIP())
		now := s.now()
		if s.ipLimiter != nil {
			if ok, wait := s.ipLimiter.take(ip, now); !ok {
				return s.reject(http.StatusTooManyRequests, codeRateLimitedClient, "ip_rate", wait)
			}
		}
		if s.inflight != nil {
			select {
			case s.inflight <- struct{}{}:
				defer func() { <-s.inflight }()
			default:
				return s.reject(http.StatusServiceUnavailable, codeOverloaded, "inflight", s.retryAfter)
			}
		}
		return next(c)
	}
}

var errUnauthorized = errors.New("unauthorized")

// authenticate enforces a static bearer token, compared in constant time, then
// applies the per-token rate limit.
//
// Failed attempts are counted per IP; once an IP exceeds its budget its failures get 429
// instead of 401. A valid token is never blocked: behind a proxy without trusted_proxies
// all clients share one IP, and blocking valid tokens there would let anyone lock out every
// caller. Guessing is not what this prevents (tokens have >= 16 characters); it bounds noise.
func (s *Server) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if s.noAuth {
			return next(c)
		}
		got, _ := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		match := -1
		for i, t := range s.tokenSums { // no early exit: time does not depend on which token matched
			if subtle.ConstantTimeCompare(sum[:], t[:]) == 1 {
				match = i
			}
		}
		now := s.now()
		if match < 0 {
			if s.failLimiter != nil {
				if ok, wait := s.failLimiter.take(clientKey(c.RealIP()), now); !ok {
					return s.reject(http.StatusTooManyRequests, codeTooManyAuthFailures, "auth_failures", wait)
				}
			}
			c.Response().Header().Set("WWW-Authenticate", "Bearer")
			return errUnauthorized
		}
		if s.tokenLimiter != nil {
			if ok, wait := s.tokenLimiter.take(s.tokenKeys[match], now); !ok {
				return s.reject(http.StatusTooManyRequests, codeRateLimitedToken, "token_rate", wait)
			}
		}
		return next(c)
	}
}
