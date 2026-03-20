package main

import (
	"context"
	"crypto/rand"
	"ecommerce/api"
	"ecommerce/routes"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/stripe/stripe-go/v84"
	"golang.org/x/time/rate"
)

//go:generate npm run build

//go:embed static
var static embed.FS

//go:embed templates
var templates embed.FS

var (
	ErrMissingEnvVar = errors.New("environment variable is required")
	ErrMissingConfig = errors.New("missing required configuration (DB_URL, RESEND_KEY, or RESEND_EMAIL)")
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(context.Background(), "tcp", "localhost:80")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
			os.Exit(1)
		}

		if err := conn.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close health check connection: %v\n", err)
		}
		os.Exit(0)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	err := run(logger)
	if err != nil {
		logger.Error("Application startup failed", "error", err)
		os.Exit(1)
	}
}

func getSecretOrEnv(envVar string, fileVar string) string {
	if path := os.Getenv(fileVar); path != "" {
		cleanPath := filepath.Clean(path)
		if content, err := os.ReadFile(cleanPath); err == nil {
			return strings.TrimSpace(string(content))
		}
	}
	return os.Getenv(envVar)
}

func run(logger *slog.Logger) error {
	ctx := context.Background()
	var appVersion string
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		appVersion = strconv.FormatInt(time.Now().Unix(), 10)
	} else {
		appVersion = n.String()
	}

	err = godotenv.Load()
	if err != nil {
		logger.Warn("Error loading .env file (ignoring if in production)")
	}

	DB_URL := getSecretOrEnv("DB_URL", "DB_URL_FILE")
	RESEND_KEY := getSecretOrEnv("RESEND_KEY", "RESEND_KEY_FILE")
	RESEND_EMAIL := getSecretOrEnv("RESEND_EMAIL", "RESEND_EMAIL_FILE")
	STRIPE_KEY := getSecretOrEnv("STRIPE_SECRET_KEY", "STRIPE_KEY_FILE")
	stripe.Key = STRIPE_KEY

	if DB_URL == "" || RESEND_KEY == "" || RESEND_EMAIL == "" || STRIPE_KEY == "" {
		return ErrMissingConfig
	}

	appCtx, cancelApp := context.WithCancel(ctx)
	defer cancelApp()

	db, err := pgxpool.New(context.Background(), DB_URL)
	if err != nil {
		logger.Error("Failed to open database", "error", err)
	}
	defer db.Close()

	if len(os.Args) > 3 && os.Args[1] == "-admin" {
		email := os.Args[2]
		password := os.Args[3]

		err := createAdmin(appCtx, db, email, password)
		if err != nil {
			logger.Error("Failed to create admin", "error", err)
			os.Exit(1)
		}
		logger.Info("Admin created successfully!", "email", email)
		os.Exit(0)
	}

	// Rate Limiters
	trusted := []string{""}
	apiLimiter := NewRateLimiter(rate.Limit(5), 10, trusted)
	authLimiter := NewRateLimiter(rate.Every(10*time.Second), 10, trusted)
	sellerLimiter := NewRateLimiter(rate.Limit(20), 50, trusted)

	mux := http.NewServeMux()
	mux.Handle("GET /static/", cacheStatic(http.FileServer(http.FS(static))))

	// ==========================================
	// INITIALIZE APPS FIRST
	// ==========================================
	appRoutes := &routes.App{DB: db, Templates: templates, Logger: logger, AppVersion: appVersion}
	appAPI := &api.App{DB: db, Logger: logger, AppVersion: appVersion, RESEND_KEY_ENV: RESEND_KEY, RESEND_EMAIL_ENV: RESEND_EMAIL}

	go appAPI.CleanExpiredSessions(appCtx)

	// ==========================================
	// 1. PUBLIC UI ROUTES
	// ==========================================
	mux.HandleFunc("GET /{$}", apiLimiter.Limit(appRoutes.Home, logger))
	mux.HandleFunc("GET /search", apiLimiter.Limit(appRoutes.Search, logger))
	mux.HandleFunc("GET /login", apiLimiter.Limit(appRoutes.Login, logger))
	mux.HandleFunc("GET /register", apiLimiter.Limit(appRoutes.Register, logger))
	mux.HandleFunc("GET /cart", apiLimiter.Limit(appRoutes.Cart, logger))
	mux.HandleFunc("GET /checkout", apiLimiter.Limit(appRoutes.Checkout, logger))
	mux.HandleFunc("GET /profile", apiLimiter.Limit(appRoutes.Profile, logger))
	mux.HandleFunc("GET /profile/orders", apiLimiter.Limit(appRoutes.ProfileOrders, logger))
	mux.HandleFunc("GET /profile/order/{orderID}", apiLimiter.Limit(appRoutes.ProfileOrderDetail, logger))
	mux.HandleFunc("GET /{productID}", apiLimiter.Limit(appRoutes.ProductDetail, logger))
	mux.HandleFunc("GET /profile/support", apiLimiter.Limit(appRoutes.ProfileSupport, logger))

	// ==========================================
	// 2. SECURE ADMIN UI ROUTES
	// ==========================================
	mux.HandleFunc("GET /admin/dashboard", sellerLimiter.Limit(appAPI.RequirePermission("", appRoutes.AdminDashboard), logger))
	mux.HandleFunc("GET /admin/products", sellerLimiter.Limit(appAPI.RequirePermission("products", appRoutes.AdminInventory), logger))
	mux.HandleFunc("GET /admin/product-edit/{productID}", sellerLimiter.Limit(appAPI.RequirePermission("products", appRoutes.AdminProductEdit), logger))
	mux.HandleFunc("GET /admin/orders", sellerLimiter.Limit(appAPI.RequirePermission("orders", appRoutes.AdminOrders), logger))
	mux.HandleFunc("GET /admin/order/{orderID}", sellerLimiter.Limit(appAPI.RequirePermission("orders", appRoutes.AdminOrderDetail), logger))
	mux.HandleFunc("GET /admin/employees", sellerLimiter.Limit(appAPI.RequirePermission("employees", appRoutes.AdminEmployees), logger))
	mux.HandleFunc("GET /admin/employee/{employeeID}", sellerLimiter.Limit(appAPI.RequirePermission("employees", appRoutes.AdminEmployeeDetail), logger))
	mux.HandleFunc("GET /admin/support", sellerLimiter.Limit(appAPI.RequirePermission("support", appRoutes.AdminSupport), logger))
	mux.HandleFunc("GET /admin/product/{productID}/edit", sellerLimiter.Limit(appAPI.RequirePermission("products", appRoutes.AdminProductEdit), logger))
	// ==========================================
	// 3. API ROUTES
	// ==========================================
	mux.HandleFunc("POST /api/profile/general", apiLimiter.Limit(appAPI.UpdateProfileGeneral, logger))
	mux.HandleFunc("POST /api/profile/email/request", apiLimiter.Limit(appAPI.RequestEmailChange, logger))
	mux.HandleFunc("POST /api/profile/email/verify", apiLimiter.Limit(appAPI.VerifyEmailChange, logger))
	mux.HandleFunc("POST /api/profile/address", apiLimiter.Limit(appAPI.AddAddress, logger))
	mux.HandleFunc("DELETE /api/profile/address/{id}", apiLimiter.Limit(appAPI.DeleteAddress, logger))
	mux.HandleFunc("PUT /api/profile/address/{id}/default", apiLimiter.Limit(appAPI.SetDefaultAddress, logger))
	mux.HandleFunc("POST /api/support", apiLimiter.Limit(appAPI.SubmitSupportTicket, logger))
	mux.HandleFunc("PUT /api/profile/support/{id}", apiLimiter.Limit(appAPI.UserUpdateSupportTicket, logger))

	// Admin API
	mux.HandleFunc("GET /api/admin/context", apiLimiter.Limit(appAPI.RequirePermission("", appAPI.SellerContextData), logger))
	mux.HandleFunc("POST /api/admin/employee/invite", apiLimiter.Limit(appAPI.InviteSellerEmployee, logger))
	mux.HandleFunc("PUT /api/admin/employee/{id}", apiLimiter.Limit(appAPI.UpdateSellerEmployee, logger))
	mux.HandleFunc("DELETE /api/admin/employee/{id}", apiLimiter.Limit(appAPI.RemoveSellerEmployee, logger))
	mux.HandleFunc("PUT /api/admin/support/{id}", sellerLimiter.Limit(appAPI.UpdateSupportTicket, logger))

	// Catalog API
	mux.HandleFunc("POST /api/review/{productID}", apiLimiter.Limit(appAPI.Review, logger))
	mux.HandleFunc("POST /api/store", apiLimiter.Limit(appAPI.Store, logger))
	mux.HandleFunc("POST /api/admin/product-groups", sellerLimiter.Limit(appAPI.RequirePermission("products", appAPI.CreateProductGroup), logger))
	mux.HandleFunc("POST /api/admin/products", sellerLimiter.Limit(appAPI.RequirePermission("products", appAPI.CreateProduct), logger))
	mux.HandleFunc("GET /api/products", apiLimiter.Limit(appAPI.ProductListAPI, logger))
	mux.HandleFunc("GET /api/store/products", apiLimiter.Limit(appAPI.StoreProductsAPI, logger))
	mux.HandleFunc("GET /api/search", apiLimiter.Limit(appAPI.SearchProductAPI, logger))
	mux.HandleFunc("POST /api/send-cart-data", apiLimiter.Limit(appAPI.SendCartData, logger))
	mux.HandleFunc("POST /api/send-checkout-data", apiLimiter.Limit(appAPI.SendCheckoutData, logger))
	mux.HandleFunc("POST /api/orders", apiLimiter.Limit(appAPI.CreateOrder, logger))
	mux.HandleFunc("POST /api/create-payment-intent", apiLimiter.Limit(appAPI.CreatePaymentIntent, logger))
	mux.HandleFunc("PUT /api/admin/products/{productID}", sellerLimiter.Limit(appAPI.RequirePermission("products", appAPI.UpdateProduct), logger))
	// Auth API
	mux.HandleFunc("POST /api/register", authLimiter.Limit(appAPI.StartRegistration, logger))
	mux.HandleFunc("POST /api/login", authLimiter.Limit(appAPI.Login, logger))
	mux.HandleFunc("POST /api/resend", authLimiter.Limit(appAPI.ResendVerificationToken, logger))
	mux.HandleFunc("POST /api/verify", authLimiter.Limit(appAPI.VerifyEmail, logger))
	mux.HandleFunc("DELETE /session/del/{name}", authLimiter.Limit(appAPI.DelSession, logger))
	mux.HandleFunc("POST /logout", authLimiter.Limit(appAPI.Logout, logger))

	// Health
	mux.HandleFunc("GET /health", apiLimiter.Limit(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte("K")); err != nil {
			logger.Error("Failed to write health response", "error", err)
		}
	}, logger))

	antiCSRF := http.NewCrossOriginProtection()
	finalHandler := antiCSRF.Handler(middleware(logger, mux))

	srv := &http.Server{
		Addr:           ":80",
		Handler:        finalHandler,
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("Started server", "address", srv.Addr)
		serverErrors <- srv.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		logger.Error("Server error", "error", err)
	case sig := <-shutdown:
		logger.Info("Signal received, starting graceful shutdown...", "signal", sig.String())
		cancelApp()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("Graceful shutdown failed", "error", err)
			if err := srv.Close(); err != nil {
				logger.Error("Server force close failed", "error", err)
			}
		}
		logger.Info("Server shut down gracefully")
	}
	return nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "deny")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		csp := "default-src 'self'; " +
			"img-src 'self' data: https:; " +
			"media-src 'self' https:; " +
			"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://js.stripe.com https://static.cloudflareinsights.com; " +
			"style-src 'self' 'unsafe-inline'; " +
			"frame-src 'self' https://js.stripe.com https://hooks.stripe.com; " +
			"connect-src 'self' https://api.stripe.com https://*.stripe.com https://cloudflareinsights.com;"
		w.Header().Set("Content-Security-Policy", csp)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if err := recover(); err != nil {
				logger.Error("Panic recovered", "error", err, "path", r.URL.Path, "method", r.Method)
				http.Error(sw, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(sw, r)
		duration := time.Since(start)
		logger.Info("HTTP Request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", duration.Seconds()*1000,
		)
	})
}

type RateLimiter struct {
	ips       map[string]*ipLimiter
	mu        sync.Mutex
	allowlist map[string]bool
	r         rate.Limit
	b         int
}

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(r rate.Limit, b int, trustedIPs []string) *RateLimiter {
	allowed := make(map[string]bool)
	for _, ip := range trustedIPs {
		allowed[ip] = true
	}

	rl := &RateLimiter{
		ips:       make(map[string]*ipLimiter),
		allowlist: allowed,
		r:         r,
		b:         b,
	}

	go func() {
		for range time.Tick(1 * time.Hour) {
			rl.mu.Lock()
			for ip, il := range rl.ips {
				if time.Since(il.lastSeen) > 1*time.Hour {
					delete(rl.ips, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()

	return rl
}

func (rl *RateLimiter) Limit(next http.HandlerFunc, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("CF-Connecting-IP")
		if ip == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				ip = host
			} else {
				ip = r.RemoteAddr
			}
		}

		if rl.allowlist[ip] {
			next(w, r)
			return
		}

		rl.mu.Lock()
		lim, exists := rl.ips[ip]
		if !exists {
			lim = &ipLimiter{
				limiter: rate.NewLimiter(rl.r, rl.b),
			}
			rl.ips[ip] = lim
		}
		lim.lastSeen = time.Now()
		allowed := lim.limiter.Allow()
		rl.mu.Unlock()

		if !allowed {
			logger.Warn("Rate limit exceeded", "ip", ip, "path", r.URL.Path, "tier_rate", rl.r)
			http.Error(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
			return
		}

		next(w, r)
	}
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

func createAdmin(ctx context.Context, db *pgxpool.Pool, email, password string) error {
	hashedPassword, err := api.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	var userID int64
	err = db.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, verified) 
		VALUES ('Store Admin', $1, $2, true)
		ON CONFLICT (email) DO UPDATE SET 
			password_hash = EXCLUDED.password_hash,
			verified = true
		RETURNING id
	`, email, hashedPassword).Scan(&userID)
	if err != nil {
		return fmt.Errorf("creating admin user: %w", err)
	}

	rolesJSON := `["Admin"]`
	_, err = db.Exec(ctx, `
		INSERT INTO employees (user_id, email, roles, status, joined_at) 
		VALUES ($1, $2, $3, 'active', NOW())
		ON CONFLICT (email) DO UPDATE SET 
			user_id = EXCLUDED.user_id, 
			roles = EXCLUDED.roles, 
			status = 'active'
	`, userID, email, rolesJSON)

	if err != nil {
		return fmt.Errorf("linking admin employee: %w", err)
	}

	return nil
}
