package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/crawler"
	"github.com/OWNER/siteinfo/internal/observability"
	"github.com/OWNER/siteinfo/internal/urlnorm"
)

// API error codes.
const (
	codeInvalidURL    = "invalid_url"
	codeInvalidParam  = "invalid_parameter"
	codeUnauthorized  = "unauthorized"
	codeNotFound      = "not_found"
	codeUnavailable   = "unavailable"
	codeRateLimited   = "upstream_rate_limited"
	codeTimeout       = "timeout"
	codeUpstream      = "upstream_error"
	codeClientClosed  = "client_closed_request"
	codeInternal      = "internal_error"
	codeNoImprint     = "imprint_not_found"
	codeMethodNotAllo = "method_not_allowed"

	// statusClientClosed is nginx's non-standard "client closed request".
	statusClientClosed = 499
)

type apiError struct {
	status     int
	code       string
	message    string
	retryAfter bool
}

// errInvalidParam is returned for malformed query parameters.
var errInvalidParam = errors.New("invalid parameter")

type paramError struct{ msg string }

func (e paramError) Error() string { return e.msg }
func (paramError) Is(t error) bool { return t == errInvalidParam }

type notFoundError struct{ code, msg string }

func (e notFoundError) Error() string { return e.msg }

// mapError translates any error to the API error shape. Messages are fixed
// strings: upstream and Redis details never reach clients.
func mapError(err error) apiError {
	var cached *cache.CachedError
	var nf notFoundError
	switch {
	case errors.Is(err, urlnorm.ErrInvalidURL):
		return apiError{status: http.StatusUnprocessableEntity, code: codeInvalidURL, message: err.Error()}
	case errors.Is(err, errInvalidParam):
		return apiError{status: http.StatusUnprocessableEntity, code: codeInvalidParam, message: err.Error()}
	case errors.Is(err, errUnauthorized):
		return apiError{status: http.StatusUnauthorized, code: codeUnauthorized, message: "missing or invalid bearer token"}
	case errors.As(err, &nf):
		return apiError{status: http.StatusNotFound, code: nf.code, message: nf.msg}
	case errors.As(err, &cached):
		return fromCode(cached.Code)
	case errors.Is(err, cache.ErrLockTimeout), errors.Is(err, cache.ErrStoreUnavailable), errors.Is(err, cache.ErrClosed):
		return apiError{status: http.StatusServiceUnavailable, code: codeUnavailable, message: "service is busy or shutting down, retry later", retryAfter: true}
	case errors.Is(err, crawler.ErrRateLimited):
		return fromCode(crawler.CodeRateLimited)
	case errors.Is(err, crawler.ErrUpstreamTimeout), errors.Is(err, context.DeadlineExceeded):
		return fromCode(crawler.CodeTimeout)
	case errors.Is(err, crawler.ErrUpstream):
		return fromCode(crawler.CodeUpstream)
	case errors.Is(err, context.Canceled):
		return apiError{status: statusClientClosed, code: codeClientClosed, message: "client closed the request"}
	}
	switch code := echo.StatusCode(err); code {
	case http.StatusNotFound:
		return apiError{status: code, code: codeNotFound, message: "route not found"}
	case http.StatusMethodNotAllowed:
		return apiError{status: code, code: codeMethodNotAllo, message: "method not allowed"}
	case 0:
	default:
		return apiError{status: code, code: http.StatusText(code), message: http.StatusText(code)}
	}
	return apiError{status: http.StatusInternalServerError, code: codeInternal, message: "internal error"}
}

// fromCode maps a stable crawler error code (live or replayed from cache).
func fromCode(code string) apiError {
	switch code {
	case crawler.CodeTimeout:
		return apiError{status: http.StatusGatewayTimeout, code: codeTimeout, message: "the upstream crawler timed out"}
	case crawler.CodeRateLimited:
		return apiError{status: http.StatusServiceUnavailable, code: codeRateLimited, message: "the upstream crawler is rate limited, retry later", retryAfter: true}
	default:
		return apiError{status: http.StatusBadGateway, code: codeUpstream, message: "the upstream crawler failed for this site"}
	}
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// handleError is the central echo error handler.
func (s *Server) handleError(c *echo.Context, err error) {
	if res, _ := echo.UnwrapResponse(c.Response()); res != nil && res.Committed {
		return
	}
	ae := mapError(err)
	if ae.status >= 500 && ae.code == codeInternal {
		observability.LoggerFrom(c.Request().Context(), s.log).Error("unhandled error", zap.Error(err))
	}
	if ae.retryAfter {
		c.Response().Header().Set(echo.HeaderRetryAfter, strconv.Itoa(int(math.Ceil(s.retryAfter.Seconds()))))
	}
	var body errorBody
	body.Error.Code, body.Error.Message = ae.code, ae.message
	if jerr := c.JSON(ae.status, body); jerr != nil {
		observability.LoggerFrom(c.Request().Context(), s.log).Error("writing error response failed", zap.Error(jerr))
	}
}
