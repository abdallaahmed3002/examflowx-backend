package main

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *server) profile(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == "GET" {
		var name, email, bio, academy string
		var slug *string
		err := s.db.QueryRow(r.Context(), `SELECT full_name,email,bio,academy_name,public_slug FROM users WHERE id=$1 AND suspended_at IS NULL`, id).Scan(&name, &email, &bio, &academy, &slug)
		if err != nil {
			fail(w, 404, "Account unavailable")
			return
		}
		write(w, 200, map[string]any{"name": name, "email": email, "bio": bio, "academyName": academy, "slug": slug})
		return
	}
	var p struct {
		Name        string `json:"name"`
		Bio         string `json:"bio"`
		AcademyName string `json:"academyName"`
		Slug        string `json:"slug"`
	}
	if decode(r, &p) != nil || len(strings.TrimSpace(p.Name)) < 2 || len(p.Name) > 120 || len(p.Bio) > 2000 || len(p.AcademyName) > 160 || (p.Slug != "" && (len(p.Slug) > 60 || !slugPattern.MatchString(p.Slug))) {
		fail(w, 400, "Invalid profile")
		return
	}
	var slug *string
	if p.Slug != "" {
		slug = &p.Slug
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE users SET full_name=$2,bio=$3,academy_name=$4,public_slug=$5 WHERE id=$1 AND suspended_at IS NULL`, id, strings.TrimSpace(p.Name), p.Bio, p.AcademyName, slug)
	if err != nil {
		fail(w, 409, "Public slug unavailable")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 404, "Account unavailable")
		return
	}
	_, _ = s.db.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,'profile.updated','user',$1)`, id)
	write(w, 200, map[string]bool{"saved": true})
}
func (s *server) publicTeacher(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var name, bio, academy, id string
	err := s.db.QueryRow(r.Context(), `SELECT id,full_name,bio,academy_name FROM users WHERE public_slug=$1 AND suspended_at IS NULL AND role='teacher'`, slug).Scan(&id, &name, &bio, &academy)
	if err != nil {
		fail(w, 404, "Teacher not found")
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT title,subject,description,duration_minutes,public_token FROM exams WHERE teacher_id=$1 AND status IN ('published','scheduled') AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now()) ORDER BY created_at DESC LIMIT 100`, id)
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer rows.Close()
	exams := []any{}
	for rows.Next() {
		var title, subject, description, token string
		var duration int
		if rows.Scan(&title, &subject, &description, &duration, &token) == nil {
			exams = append(exams, map[string]any{"title": title, "subject": subject, "description": description, "duration": duration, "publicToken": token})
		}
	}
	write(w, 200, map[string]any{"name": name, "bio": bio, "academyName": academy, "exams": exams})
}
func (s *server) changePassword(w http.ResponseWriter, r *http.Request, id string) {
	var p struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if decode(r, &p) != nil || len(p.NewPassword) < 12 || len(p.NewPassword) > 72 {
		fail(w, 400, "New password must be 12–72 characters")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var old string
	err = tx.QueryRow(r.Context(), `SELECT password_hash FROM users WHERE id=$1 AND suspended_at IS NULL FOR UPDATE`, id).Scan(&old)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(old), []byte(p.CurrentPassword)) != nil {
		fail(w, 403, "Current password is incorrect")
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(p.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE users SET password_hash=$2 WHERE id=$1`, id, string(hashed)); err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id); err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,'password.changed','user',$1)`, id); err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Password change unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "examflow_session", Path: "/", MaxAge: -1, HttpOnly: true})
	write(w, 200, map[string]bool{"sessionsRevoked": true})
}
