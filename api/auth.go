package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/resend/resend-go/v3"
	"golang.org/x/crypto/bcrypt"
)

type App struct {
	DB               *pgxpool.Pool
	Logger           *slog.Logger
	AppVersion       string
	RESEND_KEY_ENV   string
	RESEND_EMAIL_ENV string
}

type User struct {
	ID                int64
	Name              string
	Email             string
	PasswordHash      string
	Verified          bool
	VerificationToken sql.NullString
	TokenExpires      sql.NullInt64
}

type registrationRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// --- Password Helpers ---

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 14)
	return string(bytes), err
}

func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// --- Database & Session Helpers ---

func (app *App) UpdateVerificationToken(ctx context.Context, userID int64, token string, expires int64) error {
	_, err := app.DB.Exec(ctx, `
		UPDATE users
		SET verification_token = $1, token_expires = $2
		WHERE id = $3 AND verified = false
	`, token, expires, userID)
	if err != nil {
		return fmt.Errorf("update verification token db: %w", err)
	}
	return nil
}

func (app *App) SaveUserSession(ctx context.Context, r *http.Request, token, email string) error {
	hash := sha256.Sum256([]byte(token))
	hashedToken := hex.EncodeToString(hash[:])
	expires := time.Now().Add(30 * 24 * time.Hour).Unix()

	osName, browser := parseUserAgent(r.UserAgent())
	ip := getClientIP(r)
	location := getLocation(r, ip)

	_, err := app.DB.Exec(ctx, `
	INSERT INTO sessions (token, user_id, expires, os, browser, ip_address, location)
	VALUES ($1, (SELECT id FROM users WHERE email = $2), $3, $4, $5, $6, $7)`,
		hashedToken, email, expires, osName, browser, ip, location)

	if err != nil {
		return fmt.Errorf("save user session db: %w", err)
	}
	return nil
}

func (app *App) GetUserFromSession(ctx context.Context, rawToken string) (string, error) {
	hash := sha256.Sum256([]byte(rawToken))
	hashedToken := hex.EncodeToString(hash[:])
	var email string
	var expires int64

	row := app.DB.QueryRow(ctx, `
	SELECT users.email, sessions.expires
	FROM sessions
	JOIN users ON users.id = sessions.user_id
	WHERE sessions.token = $1`, hashedToken)

	err := row.Scan(&email, &expires)
	if err != nil {
		return "", fmt.Errorf("scan user from session: %w", err)
	}

	if time.Now().Unix() > expires {
		_, err = app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1", hashedToken)
		if err != nil {
			app.Logger.Warn("Failed to delete expired session", "error", err)
		}
		return "", pgx.ErrNoRows
	}

	return email, nil
}

func (app *App) DeleteUserSession(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	hashedToken := hex.EncodeToString(hash[:])
	_, err := app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1", hashedToken)
	if err != nil {
		return fmt.Errorf("delete user session db: %w", err)
	}
	return nil
}

func (app *App) CleanExpiredSessions(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()
	app.Logger.Info("Started session cleanup worker")

	for {
		select {
		case <-ticker.C:
			now := time.Now().Unix()
			_, err := app.DB.Exec(ctx, "DELETE FROM sessions WHERE expires < $1", now)
			if err != nil {
				app.Logger.Error("Failed to clean user sessions", "error", err)
			}
			_, err = app.DB.Exec(ctx, "DELETE FROM users WHERE verified = false AND token_expires < $1", now)
			if err != nil {
				app.Logger.Error("Failed to clean unverified users", "error", err)
			}
		case <-ctx.Done():
			app.Logger.Info("Session cleanup worker stopped")
			return
		}
	}
}

func (app *App) CreateUser(ctx context.Context, email string, passwordHash string) (*User, error) {
	parts := strings.Split(email, "@")
	name := parts[0]

	token, err := GenerateOTP()
	if err != nil {
		return nil, fmt.Errorf("failed to generate otp: %w", err)
	}
	expires := time.Now().Add(10 * time.Minute).Unix()

	var id int64
	err = app.DB.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, verified, verification_token, token_expires)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, name, email, passwordHash, false, token, expires).Scan(&id)

	if err != nil {
		return nil, fmt.Errorf("create user db: %w", err)
	}

	return &User{
		ID:                id,
		Name:              name,
		Email:             email,
		PasswordHash:      passwordHash,
		VerificationToken: sql.NullString{String: token, Valid: true},
		TokenExpires:      sql.NullInt64{Int64: expires, Valid: true},
	}, nil
}

var ErrUserNotFound = errors.New("user not found")

func (app *App) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	user := &User{}
	row := app.DB.QueryRow(ctx, "SELECT id, name, email, password_hash, verified FROM users WHERE email = $1", email)
	err := row.Scan(&user.ID, &user.Name, &user.Email, &user.PasswordHash, &user.Verified)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user by email scan: %w", err)
	}
	return user, nil
}

func (app *App) FindUserByToken(ctx context.Context, token string) (*User, error) {
	user := &User{}
	row := app.DB.QueryRow(ctx, `
		SELECT id, name, email, verified, verification_token, token_expires
		FROM users WHERE verification_token = $1
	`, token)

	err := row.Scan(
		&user.ID, &user.Name, &user.Email, &user.Verified,
		&user.VerificationToken, &user.TokenExpires,
	)
	if err != nil {
		return nil, fmt.Errorf("find user by token scan: %w", err)
	}

	return user, nil
}

func (app *App) VerifyUser(ctx context.Context, userID int64) error {
	_, err := app.DB.Exec(ctx, `
		UPDATE users
		SET verification_token = NULL, token_expires = NULL, verified = true
		WHERE id = $1
	`, userID)
	if err != nil {
		return fmt.Errorf("verify user db: %w", err)
	}
	return nil
}

func GenerateOTP() (string, error) {
	minVal := big.NewInt(100000)
	maxVal := big.NewInt(999999)
	randomNumber, err := rand.Int(rand.Reader, new(big.Int).Sub(maxVal, minVal).Add(new(big.Int).Sub(maxVal, minVal), big.NewInt(1)))
	if err != nil {
		return "", fmt.Errorf("failed to generate random number: %w", err)
	}
	otp := new(big.Int).Add(minVal, randomNumber)
	return fmt.Sprintf("%06d", otp), nil
}

// --- Auth Handlers ---

func (app *App) StartRegistration(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var req registrationRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		app.Logger.Warn("Invalid request body in StartRegistration", "error", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// FIX: Enforce lowercase emails
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email and password are required", http.StatusBadRequest)
		return
	}

	if len(req.Password) < 8 {
		http.Error(w, "Password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	_, err = mail.ParseAddress(req.Email)
	if err != nil {
		http.Error(w, "Invalid email format", http.StatusBadRequest)
		return
	}

	hashedPassword, err := HashPassword(req.Password)
	if err != nil {
		app.Logger.Error("Error hashing password", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	existingUser, err := app.GetUserByEmail(ctx, req.Email)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		app.Logger.Error("Database error checking existing user", "error", err, "email", req.Email)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if existingUser != nil {
		if existingUser.Verified {
			http.Error(w, "Email already exists", http.StatusConflict)
			return
		} else {
			token, err := GenerateOTP()
			if err != nil {
				app.Logger.Error("Error generating OTP", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			expires := time.Now().Add(10 * time.Minute).Unix()

			_, err = app.DB.Exec(ctx, `
				UPDATE users 
				SET password_hash = $1, verification_token = $2, token_expires = $3
				WHERE id = $4
			`, hashedPassword, token, expires, existingUser.ID)
			if err != nil {
				app.Logger.Error("Failed to update unverified user", "error", err, "userID", existingUser.ID)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			err = app.sendVerificationEmail(existingUser.Email, token)
			if err != nil {
				http.Error(w, "Could not send verification email", http.StatusInternalServerError)
				return
			}

			app.WriteJSON(w, r, http.StatusOK, map[string]string{
				"message": "Verification email sent. Please check your inbox.",
			})
			return
		}
	}

	user, err := app.CreateUser(ctx, req.Email, hashedPassword)
	if err != nil {
		app.Logger.Error("Failed to create user", "error", err, "email", req.Email)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	err = app.sendVerificationEmail(user.Email, user.VerificationToken.String)
	if err != nil {
		http.Error(w, "Could not send verification email. Please try again later.", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]string{
		"message": "Verification email sent. Please check your inbox.",
	})
}

func (app *App) Login(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		app.Logger.Warn("Invalid request body in Login", "error", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// FIX: Enforce lowercase emails on login
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	user, err := app.GetUserByEmail(ctx, req.Email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			http.Error(w, "Invalid email or password", http.StatusUnauthorized)
			return
		}
		app.Logger.Error("Database error getting user", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if !user.Verified {
		http.Error(w, "Please verify your email address before logging in", http.StatusForbidden)
		return
	}

	if !CheckPasswordHash(req.Password, user.PasswordHash) {
		http.Error(w, "Invalid email or password", http.StatusUnauthorized)
		return
	}

	b := make([]byte, 32)
	_, err = rand.Read(b)
	if err != nil {
		app.Logger.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	sessionToken := hex.EncodeToString(b)

	err = app.SaveUserSession(ctx, r, sessionToken, user.Email)
	if err != nil {
		app.Logger.Error("Failed to save user session", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
		HttpOnly: true,
		Path:     "/",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(30 * 24 * 60 * 60),
	})

	// FIX: Route active employees to dashboard and customers to home
	var isEmployee bool
	_ = app.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM employees WHERE user_id = $1 AND status = 'active')", user.ID).Scan(&isEmployee)

	redirectURL := "/"
	if isEmployee {
		redirectURL = "/admin/dashboard"
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]string{
		"redirect": redirectURL,
	})
}

func (app *App) ResendVerificationToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var req registrationRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		app.Logger.Warn("Invalid request body in ResendVerificationToken", "error", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// FIX: Enforce lowercase emails
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	user, err := app.GetUserByEmail(ctx, req.Email)
	if err != nil {
		app.Logger.Error("Database error getting user", "error", err, "email", req.Email)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	if user.Verified {
		http.Error(w, "Email is already verified", http.StatusBadRequest)
		return
	}

	token, err := GenerateOTP()
	if err != nil {
		app.Logger.Error("Error generating OTP", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(10 * time.Minute).Unix()

	err = app.UpdateVerificationToken(ctx, user.ID, token, expires)
	if err != nil {
		app.Logger.Error("Failed to update verification token", "error", err, "userID", user.ID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	err = app.sendVerificationEmail(user.Email, token)
	if err != nil {
		http.Error(w, "Could not send verification email", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]string{
		"message": "A new verification email has been sent.",
	})
}

func (app *App) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	type Code struct {
		Token string `json:"token"`
	}
	var req Code
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		app.Logger.Warn("Invalid request body in VerifyEmail", "error", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	token := req.Token
	if token == "" {
		http.Error(w, "Verification token is missing", http.StatusBadRequest)
		return
	}

	user, err := app.FindUserByToken(ctx, token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Invalid or expired token", http.StatusBadRequest)
			return
		}
		app.Logger.Error("Database error finding user by token", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if !user.TokenExpires.Valid || time.Now().Unix() > user.TokenExpires.Int64 {
		_, err = app.DB.Exec(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		if err != nil {
			app.Logger.Error("Failed to delete expired user", "error", err)
		}
		http.Error(w, "Verification token has expired", http.StatusBadRequest)
		return
	}

	err = app.VerifyUser(ctx, user.ID)
	if err != nil {
		app.Logger.Error("Failed to verify user", "error", err, "userID", user.ID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (app *App) Logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	err = app.DeleteUserSession(ctx, cookie.Value)
	if err != nil {
		app.Logger.Error("Error deleting session from DB", "error", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
	})

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (app *App) DelSession(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	name := r.PathValue("name")
	token, err := r.Cookie("session_token")
	if err != nil {
		http.Error(w, "User logout", http.StatusNotFound)
		return
	}

	email, err := app.GetUserFromSession(ctx, token.Value)
	if err != nil {
		http.Error(w, "Invalid session", http.StatusNotFound)
		return
	}

	user, err := app.GetUserByEmail(ctx, email)
	if err != nil || user == nil {
		app.Logger.Error("User not found", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var count int
	row := app.DB.QueryRow(ctx, `
		SELECT COUNT(*) AS session_count
		FROM sessions 
		WHERE user_id = $1;`, user.ID)

	err = row.Scan(&count)
	if err != nil {
		app.Logger.Error("Failed to count sessions", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if count <= 1 {
		http.Error(w, "Must have atleast one", http.StatusBadRequest)
		return
	} else {
		_, err := app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1 AND user_id = $2", name, user.ID)
		if err != nil {
			app.Logger.Error("Failed to delete session", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// --- General Profile Helpers ---

type UpdateGeneralReq struct {
	Name string `json:"name"`
}

func (app *App) UpdateProfileGeneral(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	email, err := app.GetUserFromSession(ctx, cookie.Value)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req UpdateGeneralReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "Name cannot be empty", http.StatusBadRequest)
		return
	}

	_, err = app.DB.Exec(ctx, "UPDATE users SET name = $1 WHERE email = $2", req.Name, email)
	if err != nil {
		app.Logger.Error("Failed to update user name", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type EmailChangeReq struct {
	NewEmail string `json:"newEmail"`
}

func (app *App) RequestEmailChange(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	currentEmail, err := app.GetUserFromSession(ctx, cookie.Value)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req EmailChangeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	req.NewEmail = strings.TrimSpace(strings.ToLower(req.NewEmail))
	if _, err := mail.ParseAddress(req.NewEmail); err != nil {
		http.Error(w, "Invalid email format", http.StatusBadRequest)
		return
	}

	if req.NewEmail == currentEmail {
		http.Error(w, "This is already your current email", http.StatusBadRequest)
		return
	}

	var existingID int64
	err = app.DB.QueryRow(ctx, "SELECT id FROM users WHERE email = $1", req.NewEmail).Scan(&existingID)
	if err == nil {
		http.Error(w, "This email is already in use by another account", http.StatusConflict)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	token, _ := GenerateOTP()
	expires := time.Now().Add(10 * time.Minute).Unix()

	_, err = app.DB.Exec(ctx, `
		UPDATE users 
		SET pending_email = $1, verification_token = $2, token_expires = $3
		WHERE email = $4`,
		req.NewEmail, token, expires, currentEmail)

	if err != nil {
		app.Logger.Error("Failed to update pending email", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	err = app.sendVerificationEmail(req.NewEmail, token)
	if err != nil {
		http.Error(w, "Failed to send verification email", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type VerifyEmailChangeReq struct {
	Token string `json:"token"`
}

func (app *App) VerifyEmailChange(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	currentEmail, err := app.GetUserFromSession(ctx, cookie.Value)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req VerifyEmailChangeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var pendingEmail, dbToken string
	var expires int64
	err = app.DB.QueryRow(ctx, `
		SELECT pending_email, verification_token, token_expires 
		FROM users WHERE email = $1`, currentEmail).Scan(&pendingEmail, &dbToken, &expires)

	if err != nil || dbToken != req.Token || time.Now().Unix() > expires {
		http.Error(w, "Invalid or expired verification code", http.StatusBadRequest)
		return
	}

	_, err = app.DB.Exec(ctx, `
		UPDATE users 
		SET email = $1, pending_email = NULL, verification_token = NULL, token_expires = NULL 
		WHERE email = $2`, pendingEmail, currentEmail)

	if err != nil {
		app.Logger.Error("Failed to finalize email change", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

var ErrEmailConfig = errors.New("email configuration error")

func parseUserAgent(ua string) (string, string) {
	var os, browser string
	uaLower := strings.ToLower(ua)

	switch {
	case strings.Contains(uaLower, "edg"):
		browser = "Edge"
	case strings.Contains(uaLower, "chrome") || strings.Contains(uaLower, "crios"):
		browser = "Chrome"
	case strings.Contains(uaLower, "firefox") || strings.Contains(uaLower, "fxios"):
		browser = "Firefox"
	case strings.Contains(uaLower, "safari") && !strings.Contains(uaLower, "chrome"):
		browser = "Safari"
	default:
		browser = "Unknown"
	}

	switch {
	case strings.Contains(uaLower, "windows"):
		os = "Windows"
	case strings.Contains(uaLower, "mac") && !strings.Contains(uaLower, "iphone") && !strings.Contains(uaLower, "ipad"):
		os = "macOS"
	case strings.Contains(uaLower, "android"):
		os = "Android"
	case strings.Contains(uaLower, "iphone") || strings.Contains(uaLower, "ipad") || strings.Contains(uaLower, "ipod"):
		os = "iOS"
	case strings.Contains(uaLower, "linux"):
		os = "Linux"
	default:
		os = "Unknown"
	}

	return os, browser
}

func getLocation(r *http.Request, ip string) string {
	cfCity := r.Header.Get("CF-IPCity")
	cfCountry := r.Header.Get("CF-IPCountry")

	if cfCity != "" && cfCountry != "" {
		return fmt.Sprintf("%s, %s", cfCity, cfCountry)
	}

	if ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "192.168.") {
		return "Local Network"
	}

	return "Unknown Location"
}

func getClientIP(r *http.Request) string {
	if ips := r.Header.Get("X-Forwarded-For"); ips != "" {
		return strings.Split(ips, ",")[0]
	}

	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return ip
}

func (app *App) WriteJSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(data)
	if err != nil {
		app.Logger.Error("JSON encoding failed", "error", err, "path", r.URL.Path)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = buf.WriteTo(w)
	if err != nil {
		app.Logger.Warn("Failed to send response to client", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (app *App) sendVerificationEmail(recipientEmail, token string) error {
	apiKey := app.RESEND_KEY_ENV
	senderEmail := app.RESEND_EMAIL_ENV

	if apiKey == "" {
		err := fmt.Errorf("RESEND_KEY not set: %w", ErrEmailConfig)
		app.Logger.Error("Email configuration error", "error", err)
		return err
	}
	if senderEmail == "" {
		err := fmt.Errorf("SENDER_EMAIL not set: %w", ErrEmailConfig)
		app.Logger.Error("Email configuration error", "error", err)
		return err
	}

	client := resend.NewClient(apiKey)

	params := &resend.SendEmailRequest{
		From:    senderEmail,
		To:      []string{recipientEmail},
		Subject: "Verify Your Email Address",
		Template: &resend.EmailTemplate{
			Id: "otp",
			Variables: map[string]any{
				"OTP": token,
			},
		},
	}

	_, err := client.Emails.Send(params)
	if err != nil {
		app.Logger.Error("Failed to send verification email", "recipient", recipientEmail, "error", err)
		return fmt.Errorf("send email resend: %w", err)
	}

	app.Logger.Info("Verification email sent", "recipient", recipientEmail)
	return nil
}
