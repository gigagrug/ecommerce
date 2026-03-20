package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SupportTicketReq struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

func (app *App) SubmitSupportTicket(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var req SupportTicketReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Basic validation
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	req.Subject = strings.TrimSpace(req.Subject)
	req.Message = strings.TrimSpace(req.Message)

	if req.Name == "" || req.Email == "" || req.Subject == "" || req.Message == "" {
		http.Error(w, "All fields are required", http.StatusBadRequest)
		return
	}

	// Insert into database
	_, err := app.DB.Exec(ctx, `
		INSERT INTO support_tickets (name, email, subject, message)
		VALUES ($1, $2, $3, $4)`,
		req.Name, req.Email, req.Subject, req.Message)

	if err != nil {
		app.Logger.Error("Failed to insert support ticket", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]string{
		"message": "Ticket submitted successfully",
	})
}

type UpdateTicketReq struct {
	Reply  string `json:"reply"`
	Status string `json:"status"`
}

func (app *App) UpdateSupportTicket(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Authorize the user (must be an active employee)
	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	isEmployee, err := app.CheckIfEmployee(ctx, userID)
	if err != nil || !isEmployee {
		http.Error(w, "Forbidden: Admin access required", http.StatusForbidden)
		return
	}

	// 2. Parse the Ticket ID (e.g., convert "TKT-0012" to int 12)
	ticketIDStr := r.PathValue("id")
	idStr := strings.TrimPrefix(ticketIDStr, "TKT-")
	ticketID, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ticket ID", http.StatusBadRequest)
		return
	}

	var req UpdateTicketReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	req.Status = strings.ToLower(req.Status)

	// 3. Update the database. If there is a reply, append it to the message history so it's not lost.
	if strings.TrimSpace(req.Reply) != "" {
		replyText := fmt.Sprintf("\n\n--- Admin Reply (%s) ---\n%s", time.Now().Format("Jan 02"), req.Reply)
		_, err = app.DB.Exec(ctx, "UPDATE support_tickets SET status = $1, message = message || $2 WHERE id = $3", req.Status, replyText, ticketID)
	} else {
		_, err = app.DB.Exec(ctx, "UPDATE support_tickets SET status = $1 WHERE id = $2", req.Status, ticketID)
	}

	if err != nil {
		app.Logger.Error("Failed to update support ticket", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type UserUpdateTicketReq struct {
	Reply        string `json:"reply"`
	MarkResolved bool   `json:"markResolved"`
}

func (app *App) UserUpdateSupportTicket(w http.ResponseWriter, r *http.Request) {
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

	// Parse Ticket ID
	ticketIDStr := r.PathValue("id")
	idStr := strings.TrimPrefix(ticketIDStr, "TKT-")
	ticketID, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ticket ID", http.StatusBadRequest)
		return
	}

	var req UserUpdateTicketReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Verify the user actually owns this ticket
	var ownerEmail string
	err = app.DB.QueryRow(ctx, "SELECT email FROM support_tickets WHERE id = $1", ticketID).Scan(&ownerEmail)
	if err != nil || ownerEmail != email {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Flow Logic: If they resolve it, close it. Otherwise, a user reply re-opens the ticket for the admin.
	newStatus := "open"
	if req.MarkResolved {
		newStatus = "resolved"
	}

	// Update Database
	if strings.TrimSpace(req.Reply) != "" {
		replyText := fmt.Sprintf("\n\n--- Customer Reply (%s) ---\n%s", time.Now().Format("Jan 02"), req.Reply)
		_, err = app.DB.Exec(ctx, "UPDATE support_tickets SET status = $1, message = message || $2 WHERE id = $3", newStatus, replyText, ticketID)
	} else {
		_, err = app.DB.Exec(ctx, "UPDATE support_tickets SET status = $1 WHERE id = $2", newStatus, ticketID)
	}

	if err != nil {
		app.Logger.Error("Failed to update support ticket by user", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
