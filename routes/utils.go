package routes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	DB         *pgxpool.Pool
	Templates  embed.FS
	Logger     *slog.Logger
	AppVersion string

	templateCache map[string]*template.Template
	cacheMutex    sync.RWMutex
}

func TimeAgo(t time.Time) string {
	d := time.Since(t)

	if d.Seconds() < 60 {
		return "just now"
	}
	if d.Minutes() < 60 {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}
	if d.Hours() < 24 {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(len(substr) == 0 || (func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				match := true
				for j := range len(substr) {
					if toLower(s[i+j]) != toLower(substr[j]) {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
			return false
		})())
}

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func (app *App) GetUserFromSession(ctx context.Context, token string) (string, error) {
	hash := sha256.Sum256([]byte(token))
	hashedToken := hex.EncodeToString(hash[:])

	var email string
	err := app.DB.QueryRow(ctx, "SELECT u.email FROM sessions s JOIN users u ON s.user_id = u.id WHERE s.token = $1", hashedToken).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("session token query: %w", err)
	}
	return email, nil
}

func (app *App) validateUserSession(r *http.Request, ctx context.Context) (int64, error) {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return 0, fmt.Errorf("session cookie missing: %w", err)
	}

	hash := sha256.Sum256([]byte(cookie.Value))
	hashedToken := hex.EncodeToString(hash[:])

	var userID int64
	var expires int64
	err = app.DB.QueryRow(ctx, "SELECT user_id, expires FROM sessions WHERE token = $1", hashedToken).Scan(&userID, &expires)
	if err != nil {
		app.Logger.Error("DB session lookup failed", "error", err, "token_hash", hashedToken)
		return 0, fmt.Errorf("querying session: %w", err)
	}
	if time.Now().Unix() > expires {
		_, _ = app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1", hashedToken)
		return 0, fmt.Errorf("session check: %w", sql.ErrNoRows)
	}
	return userID, nil
}

func (app *App) renderPage(w http.ResponseWriter, r *http.Request, pageData any, pageTmpl ...string) {
	query := r.URL.Query().Get("q")
	var isEmployee bool
	var isLoggedIn bool

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err == nil {
		if email, err := app.GetUserFromSession(ctx, cookie.Value); err == nil {
			isLoggedIn = true
			var empID int
			err := app.DB.QueryRow(ctx, `
				SELECT id 
				FROM employees 
				WHERE email = $1 AND status = 'active'
			`, email).Scan(&empID)

			if err == nil {
				isEmployee = true
			}
		}
	}

	data := struct {
		AppVersion string
		Query      string
		IsLoggedIn bool
		IsEmployee bool
		PageData   any
	}{
		AppVersion: app.AppVersion,
		Query:      query,
		IsLoggedIn: isLoggedIn,
		IsEmployee: isEmployee,
		PageData:   pageData,
	}

	cacheKey := strings.Join(pageTmpl, ",")

	app.cacheMutex.RLock()
	tmpl, exists := app.templateCache[cacheKey]
	app.cacheMutex.RUnlock()

	if !exists {
		app.cacheMutex.Lock()
		tmpl, exists = app.templateCache[cacheKey]
		if !exists {
			funcMap := template.FuncMap{
				"timeAgo": TimeAgo,
			}

			files := append([]string{"templates/layouts/shell.html"}, pageTmpl...)
			var err error
			tmpl, err = template.New("shell.html").Funcs(funcMap).ParseFS(app.Templates, files...)
			if err != nil {
				app.cacheMutex.Unlock()
				app.Logger.Error("template parsing error", "error", err, "templates", files)
				http.Error(w, "Error parsing templates", http.StatusInternalServerError)
				return
			}

			if app.templateCache == nil {
				app.templateCache = make(map[string]*template.Template)
			}
			app.templateCache[cacheKey] = tmpl
			app.Logger.Info("Template parsed and cached", "key", cacheKey)
		}
		app.cacheMutex.Unlock()
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "shell.html", data)
	if err != nil {
		app.Logger.Error("template execution error", "error", err, "templates", pageTmpl)
		http.Error(w, "Error executing template", http.StatusInternalServerError)
		return
	}

	hash := sha256.Sum256(buf.Bytes())
	etag := fmt.Sprintf(`W/"%x"`, hash)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
	w.Header().Set("Vary", "Cookie")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		app.Logger.Error("response write error", "error", err)
		http.Error(w, "Error executing template", http.StatusInternalServerError)
		return
	}
}

func (app *App) fetchProfileAddresses(ctx context.Context, userID int64) []SavedAddress {
	var addresses []SavedAddress
	addressRows, err := app.DB.Query(ctx, "SELECT id, full_name, address_line1, city, postal_code, is_default_shipping FROM addresses WHERE user_id = $1", userID)
	if err != nil {
		app.Logger.Error("failed to query addresses", "error", err)
		return addresses
	}
	defer addressRows.Close()

	for addressRows.Next() {
		var addr SavedAddress
		var fullName, street, city, zip sql.NullString
		var isDefault bool

		err := addressRows.Scan(&addr.ID, &fullName, &street, &city, &zip, &isDefault)
		if err != nil {
			app.Logger.Error("failed to scan address", "error", err)
			continue
		}

		addr.Label = fullName.String
		if addr.Label == "" {
			addr.Label = "Saved Address"
		}
		addr.Street = street.String
		addr.City = city.String
		addr.Zip = zip.String
		addr.IsDefault = isDefault
		addresses = append(addresses, addr)
	}

	if err := addressRows.Err(); err != nil {
		app.Logger.Error("address rows error", "error", err)
	}

	return addresses
}

func (app *App) fetchProfileSessions(ctx context.Context, userID int64, currentSessionHash string) []Session {
	var sessions []Session
	sessionRows, err := app.DB.Query(ctx, "SELECT token, os, browser, location, created_at FROM sessions WHERE user_id = $1 ORDER BY created_at DESC", userID)
	if err != nil {
		app.Logger.Error("failed to query sessions", "error", err)
		return sessions
	}
	defer sessionRows.Close()

	for sessionRows.Next() {
		var token, osName, browser, loc string
		var createdAt time.Time

		if err := sessionRows.Scan(&token, &osName, &browser, &loc, &createdAt); err != nil {
			app.Logger.Error("failed to scan session", "error", err)
			continue
		}
		sessions = append(sessions, Session{
			Token:     token,
			OS:        osName,
			Browser:   browser,
			Location:  loc,
			LoginDate: createdAt.Format("Jan 02, 2006 at 3:04 PM"),
			IsCurrent: token == currentSessionHash,
		})
	}

	if err := sessionRows.Err(); err != nil {
		app.Logger.Error("session rows error", "error", err)
	}

	return sessions
}
