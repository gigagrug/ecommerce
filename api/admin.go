package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	ErrNotEmployee  = errors.New("user is not an active employee")
	ErrUnauthorized = errors.New("unauthorized for this action")
)

// The available system modules an employee can be granted access to
var AvailableRoles = []string{"Admin", "products", "orders", "support", "employees"}

type SellerContextResponse struct {
	Roles []string `json:"roles"`
}

func (app *App) getEmployeeRoles(ctx context.Context, userID int64) ([]string, error) {
	var rolesJSON []byte

	err := app.DB.QueryRow(ctx, `
		SELECT roles FROM employees WHERE user_id = $1 AND status = 'active'
	`, userID).Scan(&rolesJSON)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotEmployee
		}
		return nil, fmt.Errorf("failed to query employee roles: %w", err)
	}

	var roles []string
	if err := json.Unmarshal(rolesJSON, &roles); err != nil {
		return nil, fmt.Errorf("failed to decode roles: %w", err)
	}

	return roles, nil
}

// hasRole checks if the user has the specific role OR is an Admin
func hasRole(roles []string, required string) bool {
	for _, r := range roles {
		if r == "Admin" || r == required {
			return true
		}
	}
	return false
}

func (app *App) SellerContextData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roles, err := app.getEmployeeRoles(ctx, userID)
	if err != nil {
		http.Error(w, "Forbidden: You are not an active member of the team", http.StatusForbidden)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, SellerContextResponse{Roles: roles})
}

func (app *App) RequirePermission(requiredRole string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		userID, err := app.validateUserSession(r, ctx)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		roles, err := app.getEmployeeRoles(ctx, userID)
		if err != nil {
			http.Error(w, "Forbidden: You are not an active employee", http.StatusForbidden)
			return
		}

		if requiredRole != "" && !hasRole(roles, requiredRole) {
			http.Error(w, fmt.Sprintf("Forbidden: You lack the '%s' access", requiredRole), http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// --------------------------------------------------------
// TEAM MANAGEMENT
// --------------------------------------------------------

type TeamMember struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	Roles      []string `json:"roles"`
	Status     string   `json:"status"`
	JoinedAt   string   `json:"joinedDate"`
	LastActive string   `json:"lastActive"`
}

func (app *App) GetSellerTeam(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roles, err := app.getEmployeeRoles(ctx, userID)
	if err != nil || !hasRole(roles, "employees") {
		http.Error(w, "Forbidden: missing employees access", http.StatusForbidden)
		return
	}

	query := `
		SELECT e.id, COALESCE(u.name, 'Staff Member'), e.email, e.roles, e.status, e.joined_at
		FROM employees e
		LEFT JOIN users u ON e.user_id = u.id
		ORDER BY e.created_at DESC`

	rows, err := app.DB.Query(ctx, query)
	if err != nil {
		app.Logger.Error("Failed to fetch team", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var team []TeamMember
	for rows.Next() {
		var tm TeamMember
		var joined sql.NullTime
		var rolesJSON []byte

		err := rows.Scan(&tm.ID, &tm.Name, &tm.Email, &rolesJSON, &tm.Status, &joined)
		if err == nil {
			if joined.Valid {
				tm.JoinedAt = joined.Time.Format("Jan 02, 2006")
			} else {
				tm.JoinedAt = "N/A"
			}
			tm.LastActive = "Active recently"
			_ = json.Unmarshal(rolesJSON, &tm.Roles)
			team = append(team, tm)
		}
	}

	if team == nil {
		team = []TeamMember{}
	}

	app.WriteJSON(w, r, http.StatusOK, team)
}

type CreateEmployeeReq struct {
	Email    string   `json:"email"`
	Password string   `json:"password"`
	Roles    []string `json:"roles"`
}

func (app *App) InviteSellerEmployee(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roles, err := app.getEmployeeRoles(ctx, userID)
	if err != nil || !hasRole(roles, "employees") {
		http.Error(w, "Forbidden: missing employees access", http.StatusForbidden)
		return
	}

	var req CreateEmployeeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || len(req.Password) < 8 {
		http.Error(w, "Valid email and a password of at least 8 characters are required", http.StatusBadRequest)
		return
	}

	if len(req.Roles) == 0 {
		http.Error(w, "At least one role must be selected", http.StatusBadRequest)
		return
	}

	rolesJSON, err := json.Marshal(req.Roles)
	if err != nil {
		http.Error(w, "Invalid roles format", http.StatusBadRequest)
		return
	}

	hashedPassword, err := HashPassword(req.Password)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	tx, err := app.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var newUserID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash, verified) 
		VALUES ('Staff Member', $1, $2, true)
		ON CONFLICT (email) DO UPDATE SET 
			password_hash = EXCLUDED.password_hash,
			verified = true
		RETURNING id
	`, req.Email, hashedPassword).Scan(&newUserID)

	if err != nil {
		http.Error(w, "Failed to create user account", http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO employees (user_id, email, roles, status, joined_at)
		VALUES ($1, $2, $3, 'active', NOW())`,
		newUserID, req.Email, rolesJSON)

	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "This user is already on your team", http.StatusConflict)
			return
		}
		http.Error(w, "Failed to add employee", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (app *App) RemoveSellerEmployee(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roles, err := app.getEmployeeRoles(ctx, userID)
	if err != nil || !hasRole(roles, "employees") {
		http.Error(w, "Forbidden: missing employees access", http.StatusForbidden)
		return
	}

	empID := r.PathValue("id")

	var targetUserID sql.NullInt64
	_ = app.DB.QueryRow(ctx, "SELECT user_id FROM employees WHERE id = $1", empID).Scan(&targetUserID)
	if targetUserID.Valid && targetUserID.Int64 == userID {
		http.Error(w, "You cannot remove yourself", http.StatusForbidden)
		return
	}

	_, err = app.DB.Exec(ctx, "DELETE FROM employees WHERE id = $1", empID)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type UpdateEmployeeReq struct {
	Roles  []string `json:"roles"`
	Status string   `json:"status"`
}

func (app *App) UpdateSellerEmployee(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	roles, err := app.getEmployeeRoles(ctx, userID)
	if err != nil || !hasRole(roles, "employees") {
		http.Error(w, "Forbidden: missing employees access", http.StatusForbidden)
		return
	}

	empID := r.PathValue("id")

	var req UpdateEmployeeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if len(req.Roles) == 0 {
		http.Error(w, "Employee must have at least one role", http.StatusBadRequest)
		return
	}

	rolesJSON, _ := json.Marshal(req.Roles)

	_, err = app.DB.Exec(ctx, `
		UPDATE employees 
		SET roles = $1, status = $2 
		WHERE id = $3`,
		rolesJSON, req.Status, empID)

	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
