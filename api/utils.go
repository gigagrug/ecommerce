package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	DB               *pgxpool.Pool
	Logger           *slog.Logger
	AppVersion       string
	RESEND_KEY_ENV   string
	RESEND_EMAIL_ENV string
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
