package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// audit records a security-relevant action. Failures are logged but never
// block the user-facing operation.
func (s *server) audit(actorID, action, entityType, entityID string) {
	var id *string
	if actorID != "" {
		id = &actorID
	}
	var eid *string
	if entityID != "" {
		eid = &entityID
	}
	if _, err := s.db.Exec(context.Background(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,$2,$3,$4)`, id, action, entityType, eid); err != nil {
		slog.Error("audit write failed", "action", action, "error", err)
	}
}

// forgotPassword always returns a generic response so the endpoint cannot be
// used to enumerate accounts. In this deployment (no SMTP integration) the
// reset link is written to the server log and, when configured, to
// EXAMFLOW_RESET_FILE for local recovery.
func (s *server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var p struct{ Email string }
	if decode(r, &p) != nil {
		fail(w, 400, "Invalid request")
		return
	}
	email := strings.ToLower(strings.TrimSpace(p.Email))
	generic := map[string]bool{"ok": true}
	if email == "" || len(email) > 254 {
		write(w, 200, generic)
		return
	}
	var userID string
	if err := s.db.QueryRow(r.Context(), `SELECT id FROM users WHERE email=$1 AND suspended_at IS NULL`, email).Scan(&userID); err != nil {
		write(w, 200, generic)
		return
	}
	// Single-use token, stored hashed, 30-minute expiry; older tokens revoked.
	t := token()
	if _, err := s.db.Exec(r.Context(), `UPDATE password_reset_tokens SET used_at=now() WHERE user_id=$1 AND used_at IS NULL`, userID); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if _, err := s.db.Exec(r.Context(), `INSERT INTO password_reset_tokens(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '30 minutes')`, hash(t), userID); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	link := "https://" + r.Host + "/reset-password?token=" + t
	slog.Warn("password reset requested", "email", email, "link", link)
	if path := os.Getenv("EXAMFLOW_RESET_FILE"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + email + " " + link + "\n")
			_ = f.Close()
		}
	}
	s.audit(userID, "auth.reset_requested", "user", userID)
	write(w, 200, generic)
}

// resetPassword consumes a valid token, sets the new password and revokes
// every existing session for the account.
func (s *server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var p struct{ Token, Password string }
	if decode(r, &p) != nil || len(p.Password) < 12 || len(p.Password) > 72 || p.Token == "" || len(p.Token) > 128 {
		fail(w, 400, "Valid token and password of at least 12 characters required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var userID string
	err = tx.QueryRow(r.Context(), `SELECT user_id FROM password_reset_tokens WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now() FOR UPDATE`, hash(p.Token)).Scan(&userID)
	if err != nil {
		fail(w, 400, "Reset link is invalid or has expired")
		return
	}
	b, _ := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if _, err = tx.Exec(r.Context(), `UPDATE users SET password_hash=$2 WHERE id=$1`, userID, string(b)); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE password_reset_tokens SET used_at=now() WHERE token_hash=$1`, hash(p.Token)); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	s.audit(userID, "auth.password_reset", "user", userID)
	write(w, 200, map[string]bool{"ok": true})
}
