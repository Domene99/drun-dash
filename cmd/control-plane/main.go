// Command control-plane runs the drun-dash REST API and admin UI server.
//
// This is a v1 scaffold: it exposes liveness/readiness endpoints, the
// authentication vertical (POST /auth/session and GET /auth/whoami), and
// graceful shutdown. Readiness is backed by a database ping; the admin UI
// (templ+HTMX) and resource handlers arrive in later batches.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/health"
	"github.com/Domene99/drun-dash/internal/httpapi"
	"github.com/Domene99/drun-dash/internal/httpserver"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/log"
	"github.com/Domene99/drun-dash/internal/oidc"
	"github.com/Domene99/drun-dash/internal/redis"
	"github.com/Domene99/drun-dash/internal/store"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

// Auth environment variables and their defaults. internal/config is frozen for
// this batch, so the control plane reads its auth settings straight from the
// environment (documented in cmd/control-plane/README.md).
const (
	envJWTIssuer      = "CP_JWT_ISSUER"
	envJWTAudience    = "CP_JWT_AUDIENCE"
	envJWTKID         = "CP_JWT_KID"
	envJWTSigningSeed = "CP_JWT_SIGNING_SEED"
	envAccessTokenTTL = "CP_ACCESS_TOKEN_TTL"
	envOIDCIssuer     = "CP_OIDC_ISSUER"
	envOIDCClientID   = "CP_OIDC_CLIENT_ID"

	// Device-authorization flow (ADR-0001 §2): the admin UI origin used to
	// build the device-flow verification_uri (<base>/device) shown to the user.
	envAdminUIBaseURL = "CP_ADMIN_UI_BASE_URL"

	// Session-launch settings (ADR-0003): the bundle-signing key and its id,
	// the proxy base URL handed to launched sessions, and the session lifetime
	// governing both the bundle and the grant expiry.
	envBundleSigningSeed = "CP_BUNDLE_SIGNING_SEED"
	envBundleSigningKID  = "CP_BUNDLE_SIGNING_KID"
	envProxyBaseURL      = "CP_PROXY_BASE_URL"
	envSessionTTL        = "CP_SESSION_TTL"

	defaultJWTIssuer      = "drun-dash"
	defaultJWTAudience    = "drun-dash-api"
	defaultJWTKID         = "dev"
	defaultAccessTokenTTL = 15 * time.Minute

	defaultBundleSigningKID = "dev"
	defaultAdminUIBaseURL   = "http://localhost:8080"
	defaultProxyBaseURL     = "http://localhost:8081"
	defaultSessionTTL       = 60 * time.Minute
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("control-plane exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads configuration, opens the database and Redis pools, assembles the
// auth stack, builds the router, and serves until an interrupt or termination
// signal triggers a graceful shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)
	logger.Info("control-plane starting", slog.String("version", version), slog.String("env", cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect is lazy: it validates the connection string but does not dial, so
	// this succeeds even when Postgres is momentarily down. The /readyz check
	// pings the pool to report actual connectivity.
	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Redis backs the token revocation denylist. Connect is likewise lazy.
	redisClient, err := redis.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()

	authDeps, err := buildAuthDeps(ctx, logger, pool, redisClient)
	if err != nil {
		return err
	}

	router := newRouter(logger, authDeps, health.Named{Name: "database", Checker: health.PingChecker{Pool: pool}})
	return httpserver.New(cfg, router, logger).Run(ctx)
}

// buildAuthDeps assembles the authentication stack from the environment: the
// session-token issuer and verifier (sharing one Ed25519 key), the Redis-backed
// revoker, the member store, and the OIDC verifier for the session exchange. It
// also carries the shared database pool so the resource handlers can build
// their own stores (store.NewAgentStore(d.Pool), …) without this function
// growing a field per resource, and the session-launch material (bundle-signing
// key, proxy URL, session TTL).
//
// The OIDC verifier requires live discovery against the org's IdP, so it is
// built only when CP_OIDC_ISSUER and CP_OIDC_CLIENT_ID are both set. When they
// are not, auth is not wired (a warning is logged) and the server still serves
// health checks — this keeps a bare local `go run` working without an IdP.
func buildAuthDeps(ctx context.Context, logger *slog.Logger, pool *db.Pool, redisClient *redis.Client) (*httpapi.Deps, error) {
	issuerName := getEnv(envJWTIssuer, defaultJWTIssuer)
	audience := getEnv(envJWTAudience, defaultJWTAudience)
	kid := getEnv(envJWTKID, defaultJWTKID)

	ttl := defaultAccessTokenTTL
	if raw := os.Getenv(envAccessTokenTTL); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("control-plane: %s: %w", envAccessTokenTTL, err)
		}
		ttl = parsed
	}

	signingKey, err := signingKeyFromEnv(logger)
	if err != nil {
		return nil, err
	}
	publicKey := signingKey.Public().(ed25519.PublicKey)

	issuer, err := auth.NewIssuer(auth.IssuerConfig{
		SigningKey: signingKey,
		KID:        kid,
		Issuer:     issuerName,
		Audience:   audience,
		TTL:        ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("control-plane: build issuer: %w", err)
	}

	revoker, err := auth.NewRedisRevoker(redisClient)
	if err != nil {
		return nil, fmt.Errorf("control-plane: build revoker: %w", err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{kid: publicKey},
		Issuer:   issuerName,
		Audience: audience,
		Revoker:  revoker,
	})
	if err != nil {
		return nil, fmt.Errorf("control-plane: build verifier: %w", err)
	}

	bundleKey, err := bundleSigningKeyFromEnv(logger)
	if err != nil {
		return nil, err
	}
	bundleKID := getEnv(envBundleSigningKID, defaultBundleSigningKID)
	proxyBaseURL := getEnv(envProxyBaseURL, defaultProxyBaseURL)
	adminUIBaseURL := getEnv(envAdminUIBaseURL, defaultAdminUIBaseURL)

	sessionTTL := defaultSessionTTL
	if raw := os.Getenv(envSessionTTL); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("control-plane: %s: %w", envSessionTTL, err)
		}
		sessionTTL = parsed
	}

	oidcIssuer := os.Getenv(envOIDCIssuer)
	oidcClientID := os.Getenv(envOIDCClientID)
	if oidcIssuer == "" || oidcClientID == "" {
		logger.Warn("OIDC not configured; auth routes disabled",
			slog.String("hint", fmt.Sprintf("set %s and %s to enable /auth/session and /auth/whoami", envOIDCIssuer, envOIDCClientID)))
		return nil, nil
	}
	oidcVerifier, err := oidc.NewRemoteVerifier(ctx, oidcIssuer, oidcClientID)
	if err != nil {
		return nil, fmt.Errorf("control-plane: build oidc verifier: %w", err)
	}

	logger.Info("auth configured",
		slog.String("issuer", issuerName),
		slog.String("audience", audience),
		slog.String("kid", kid),
		slog.Duration("access_token_ttl", ttl),
		slog.String("oidc_issuer", oidcIssuer),
		slog.String("bundle_kid", bundleKID),
		slog.String("proxy_base_url", proxyBaseURL),
		slog.String("admin_ui_base_url", adminUIBaseURL),
		slog.Duration("session_ttl", sessionTTL),
	)

	return &httpapi.Deps{
		OIDC:             oidcVerifier,
		Issuer:           issuer,
		Verifier:         verifier,
		Members:          store.NewMemberStore(pool),
		Redis:            redisClient,
		AdminUIBaseURL:   adminUIBaseURL,
		Pool:             pool,
		AccessTTL:        ttl,
		BundleSigningKey: bundleKey,
		BundleSigningKID: bundleKID,
		ProxyBaseURL:     proxyBaseURL,
		SessionTTL:       sessionTTL,
	}, nil
}

// signingKeyFromEnv builds the Ed25519 signing key from CP_JWT_SIGNING_SEED (a
// base64-encoded 32-byte seed). When the variable is unset, an ephemeral key is
// generated so local development works out of the box; tokens minted with it do
// not survive a restart, which is fine for dev and unacceptable for production
// (hence the warning). An explicitly set but malformed seed is a hard error.
func signingKeyFromEnv(logger *slog.Logger) (ed25519.PrivateKey, error) {
	return seedKeyFromEnv(logger, envJWTSigningSeed, "signing seed", "tokens will not survive restart")
}

// bundleSigningKeyFromEnv builds the Ed25519 policy-bundle signing key from
// CP_BUNDLE_SIGNING_SEED, with the same seed handling as the JWT key: an unset
// seed yields an ephemeral key (dev convenience; bundles signed with it verify
// only against the ephemeral public half), and a malformed seed is a hard
// error. It is a distinct key from the JWT signing key (ADR-0003 §2).
func bundleSigningKeyFromEnv(logger *slog.Logger) (ed25519.PrivateKey, error) {
	return seedKeyFromEnv(logger, envBundleSigningSeed, "bundle signing seed", "bundles will not verify across restarts")
}

// seedKeyFromEnv builds an Ed25519 private key from a base64-encoded 32-byte
// seed in the named environment variable. An unset variable generates an
// ephemeral key (logging a warning that names the consequence); a set but
// malformed seed is a hard error.
func seedKeyFromEnv(logger *slog.Logger, envVar, label, consequence string) (ed25519.PrivateKey, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		logger.Warn(fmt.Sprintf("no %s set; generating an ephemeral key (%s)", label, consequence),
			slog.String("hint", fmt.Sprintf("set %s to a base64-encoded 32-byte seed in production", envVar)))
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("control-plane: generate ephemeral %s: %w", label, err)
		}
		return priv, nil
	}

	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("control-plane: %s: decode base64: %w", envVar, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("control-plane: %s: seed must decode to %d bytes, got %d", envVar, ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// getEnv returns the value of key, or def when the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newRouter builds the HTTP router for the control-plane server, wrapping every
// route in the shared middleware chain: request IDs, panic recovery, and
// structured access logging. The readiness checkers back GET /readyz. When
// authDeps is non-nil, the auth routes are mounted via httpapi.Mount.
func newRouter(logger *slog.Logger, authDeps *httpapi.Deps, checkers ...health.Named) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	r.Get("/healthz", healthz)
	r.Get("/readyz", health.Handler(checkers...))

	if authDeps != nil {
		httpapi.Mount(r, *authDeps)
	}
	return r
}

// healthz reports server liveness. It is a static 200 and deliberately does not
// consult downstream dependencies, so a dependency blip cannot make an
// orchestrator kill an otherwise-healthy process.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
