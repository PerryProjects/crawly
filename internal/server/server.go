// Package server wires the Echo HTTP API: routes, middleware, handlers and
// error mapping. It depends on small consumer-side interfaces only.
package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"

	"github.com/OWNER/siteinfo/internal/brand"
	"github.com/OWNER/siteinfo/internal/cache"
	"github.com/OWNER/siteinfo/internal/imprint"
)

// ImprintLookup loads an imprint from upstream (the cache miss path).
type ImprintLookup interface {
	Lookup(ctx context.Context, host string) (*imprint.Imprint, error)
}

// BrandLookup loads brand data from upstream (the cache miss path).
type BrandLookup interface {
	Lookup(ctx context.Context, host string) (*brand.Brand, error)
}

// Pinger checks the cache backend for the readiness probe.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RequestObserver records HTTP request metrics and protection rejections.
// It is implemented by *observability.Metrics.
type RequestObserver interface {
	ObserveRequest(route, method string, status int, d time.Duration)
	Rejected(reason string)
}

type noopRequestObserver struct{}

func (noopRequestObserver) ObserveRequest(string, string, int, time.Duration) {}
func (noopRequestObserver) Rejected(string)                                   {}

// Protection configures overload and abuse protection for the /v1 routes.
// A zero rate or limit disables that check. It is defense in depth, not a
// replacement for edge protection against volumetric DDoS.
type Protection struct {
	IPRate, TokenRate     float64 // requests per second per client IP / per API token
	IPBurst, TokenBurst   int
	AuthFailuresPerMinute float64 // failed authentications per IP before it is blocked
	MaxInflight           int     // concurrent /v1 requests in this pod
	MaxTrackedKeys        int     // bound on limiter memory
}

// ErrNoTokens is returned by New when auth is enabled but no token is configured.
var ErrNoTokens = errors.New("server: auth is enabled but no tokens are configured")

// Server is the HTTP API.
type Server struct {
	echo      *echo.Echo
	loader    cache.Loader
	imprints  ImprintLookup
	brands    BrandLookup
	ready     Pinger
	tokenSums [][sha256.Size]byte
	tokenKeys []string // limiter key per token index (never the token itself)
	noAuth    bool

	log            *zap.Logger
	sugar          *zap.SugaredLogger // everything except the zero-allocation access log
	obs            RequestObserver
	handlerTimeout time.Duration
	retryAfter     time.Duration
	readyTimeout   time.Duration
	refreshEnabled bool
	trustedProxies []*net.IPNet

	ipLimiter    *keyedLimiter
	tokenLimiter *keyedLimiter
	failLimiter  *keyedLimiter
	inflight     chan struct{}
	now          func() time.Time
	entries      sync.Pool // *accessEntry, so the access log allocates nothing per request

	shuttingDown atomic.Bool
}

// Option configures optional Server settings.
type Option func(*Server)

// WithLogger sets the base logger.
func WithLogger(l *zap.Logger) Option { return func(s *Server) { s.log = l } }

// WithObserver sets the HTTP metrics sink.
func WithObserver(o RequestObserver) Option { return func(s *Server) { s.obs = o } }

// WithHandlerTimeout sets the context deadline of every lookup request (default 45s).
func WithHandlerTimeout(d time.Duration) Option { return func(s *Server) { s.handlerTimeout = d } }

// WithRetryAfter sets the Retry-After value sent with 503 responses (default 5s).
func WithRetryAfter(d time.Duration) Option { return func(s *Server) { s.retryAfter = d } }

// WithRefresh allows the refresh=true query parameter (disabled by default).
func WithRefresh(enabled bool) Option { return func(s *Server) { s.refreshEnabled = enabled } }

// WithProtection enables rate limiting and load shedding on the /v1 routes.
func WithProtection(p Protection) Option {
	return func(s *Server) {
		keys := max(p.MaxTrackedKeys, 1000)
		s.ipLimiter = newKeyedLimiter(p.IPRate, p.IPBurst, keys)
		s.tokenLimiter = newKeyedLimiter(p.TokenRate, p.TokenBurst, keys)
		s.failLimiter = newKeyedLimiter(p.AuthFailuresPerMinute/60, int(math.Ceil(p.AuthFailuresPerMinute)), keys)
		if p.MaxInflight > 0 {
			s.inflight = make(chan struct{}, p.MaxInflight)
		}
	}
}

// WithTrustedProxies makes the client IP come from X-Forwarded-For, believing only
// the given proxy ranges. Without it the TCP peer address is used, because a
// spoofable header would defeat per-IP limits.
func WithTrustedProxies(nets []*net.IPNet) Option { return func(s *Server) { s.trustedProxies = nets } }

// WithoutAuth disables bearer authentication. This is for local development only.
func WithoutAuth() Option { return func(s *Server) { s.noAuth = true } }

// New builds the server. Bearer authentication is enforced unless WithoutAuth
// is passed; tokens must then contain at least one token.
func New(loader cache.Loader, imprints ImprintLookup, brands BrandLookup, ready Pinger, tokens []string, opts ...Option) (*Server, error) {
	s := &Server{
		loader: loader, imprints: imprints, brands: brands, ready: ready,
		log: zap.NewNop(), obs: noopRequestObserver{},
		handlerTimeout: 45 * time.Second, retryAfter: 5 * time.Second, readyTimeout: time.Second,
		now: time.Now,
	}
	s.entries.New = func() any { return new(accessEntry) }
	for _, o := range opts {
		o(s)
	}
	s.sugar = s.log.Sugar()
	if !s.noAuth {
		if len(tokens) == 0 {
			return nil, ErrNoTokens
		}
		// Hashing makes the comparison independent of token length.
		for _, t := range tokens {
			s.tokenSums = append(s.tokenSums, sha256.Sum256([]byte(t)))
			s.tokenKeys = append(s.tokenKeys, "token-"+strconv.Itoa(len(s.tokenKeys)))
		}
	}

	cfg := echo.Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)), // we log with zap
		HTTPErrorHandler: s.handleError,
	}
	if len(s.trustedProxies) > 0 {
		opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
		for _, n := range s.trustedProxies {
			opts = append(opts, echo.TrustIPRange(n))
		}
		cfg.IPExtractor = echo.ExtractIPFromXFFHeader(opts...)
	}
	s.echo = echo.NewWithConfig(cfg)
	s.echo.Use(s.requestID, s.accessLog, s.recoverPanic)

	s.echo.GET("/healthz", s.healthz)
	s.echo.GET("/readyz", s.readyz)
	// Auth is attached per route (not via a Group) so a wrong method still yields 405.
	// Order: shed/limit cheaply first (per IP, in-flight cap), then authenticate, then per-token limit.
	s.echo.GET("/v1/imprint", s.getImprint, s.protect, s.authenticate)
	s.echo.GET("/v1/brand", s.getBrand, s.protect, s.authenticate)
	return s, nil
}

// Handler returns the http.Handler to serve.
func (s *Server) Handler() http.Handler { return s.echo }

// MarkNotReady makes /readyz fail from now on. Called first on shutdown so the
// pod is removed from the Service endpoints before connections are drained.
func (s *Server) MarkNotReady() { s.shuttingDown.Store(true) }

func (s *Server) healthz(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) // liveness: no dependencies
}

func (s *Server) readyz(c *echo.Context) error {
	if s.shuttingDown.Load() {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), s.readyTimeout)
	defer cancel()
	if err := s.ready.Ping(ctx); err != nil {
		s.sugar.Warnw("readiness check failed", "error", err)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "redis_unavailable"})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
