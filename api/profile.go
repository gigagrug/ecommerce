package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (app *App) GetUserIDFromSession(ctx context.Context, rawToken string) (int64, error) {
	hash := sha256.Sum256([]byte(rawToken))
	hashedToken := hex.EncodeToString(hash[:])

	var userID int64
	var expires int64

	err := app.DB.QueryRow(ctx, `SELECT user_id, expires FROM sessions WHERE token = $1`, hashedToken).Scan(&userID, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("session not found: %w", err)
		}
		return 0, fmt.Errorf("session scan: %w", err)
	}

	if time.Now().Unix() > expires {
		_, err := app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1", hashedToken)
		if err != nil {
			app.Logger.Warn("Failed to delete expired session", "error", err)
		}
		return 0, sql.ErrNoRows
	}

	return userID, nil
}

func (app *App) validateUserSession(r *http.Request, ctx context.Context) (int64, error) {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return 0, fmt.Errorf("missing session cookie: %w", err)
	}

	userID, err := app.GetUserIDFromSession(ctx, cookie.Value)
	if err != nil {
		return 0, fmt.Errorf("invalid or expired session: %w", err)
	}

	return userID, nil
}

func (app *App) CheckIfEmployee(ctx context.Context, userID int64) (bool, error) {
	var exists bool
	err := app.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM employees WHERE user_id = $1 AND status = 'active')", userID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (app *App) Store(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	isEmployee, err := app.CheckIfEmployee(ctx, userID)
	if err != nil || !isEmployee {
		http.Error(w, "Access Forbidden: Admin access required", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Successfully updated store settings!"))
}

type AddressReq struct {
	Label    string `json:"label"`
	FullName string `json:"fullName"`
	Street1  string `json:"street1"`
	Street2  string `json:"street2"`
	City     string `json:"city"`
	State    string `json:"state"`
	Zip      string `json:"zip"`
	Country  string `json:"country"`
	Phone    string `json:"phone"`
}

func (app *App) AddAddress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req AddressReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Street1) == "" || strings.TrimSpace(req.City) == "" {
		http.Error(w, "Street and City are required", http.StatusBadRequest)
		return
	}

	var count int
	if err := app.DB.QueryRow(ctx, "SELECT COUNT(*) FROM addresses WHERE user_id = $1", userID).Scan(&count); err != nil {
		app.Logger.Warn("Failed to retrieve address count", "error", err)
	}
	isDefault := count == 0

	var newID int64
	err = app.DB.QueryRow(ctx, `
		INSERT INTO addresses (
			user_id, full_name, address_line1, address_line2, 
			city, state, postal_code, country, phone, is_default_shipping
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		userID, req.FullName, req.Street1, req.Street2,
		req.City, req.State, req.Zip, req.Country, req.Phone, isDefault).Scan(&newID)

	if err != nil {
		app.Logger.Error("Failed to insert address", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]any{"id": newID, "isDefault": isDefault})
}

func (app *App) DeleteAddress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	addressID := r.PathValue("id")

	_, err = app.DB.Exec(ctx, "DELETE FROM addresses WHERE id = $1 AND user_id = $2", addressID, userID)
	if err != nil {
		app.Logger.Error("Failed to delete address", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (app *App) SetDefaultAddress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	addressID := r.PathValue("id")

	tx, err := app.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, "UPDATE addresses SET is_default_shipping = false WHERE user_id = $1", userID)
	if err != nil {
		app.Logger.Error("Failed to clear default address", "error", err)
		return
	}

	_, err = tx.Exec(ctx, "UPDATE addresses SET is_default_shipping = true WHERE id = $1 AND user_id = $2", addressID, userID)
	if err != nil {
		app.Logger.Error("Failed to set new default address", "error", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		app.Logger.Error("Failed to commit default address transaction", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
