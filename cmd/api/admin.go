package main

import (
	"github.com/go-chi/chi/v5"
	"net/http"
)

func (s *server) admin(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return s.protected(func(w http.ResponseWriter, r *http.Request, id string) {
		var role string
		if s.db.QueryRow(r.Context(), `SELECT role FROM users WHERE id=$1 AND suspended_at IS NULL`, id).Scan(&role) != nil || role != "admin" {
			fail(w, 403, "Admin access required")
			return
		}
		fn(w, r, id)
	})
}
func (s *server) adminOverview(w http.ResponseWriter, r *http.Request, id string) {
	var users, suspended, admins, proUsers, exams, published, attempts, students, classes int
	err := s.db.QueryRow(r.Context(), `SELECT
		(SELECT count(*) FROM users),
		(SELECT count(*) FROM users WHERE suspended_at IS NOT NULL),
		(SELECT count(*) FROM users WHERE role='admin'),
		(SELECT count(*) FROM users WHERE plan='pro' AND (plan_expires_at IS NULL OR plan_expires_at>now())),
		(SELECT count(*) FROM exams),
		(SELECT count(*) FROM exams WHERE status IN ('published','scheduled')),
		(SELECT count(*) FROM attempts),
		(SELECT count(*) FROM students),
		(SELECT count(*) FROM classes)`).
		Scan(&users, &suspended, &admins, &proUsers, &exams, &published, &attempts, &students, &classes)
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	write(w, 200, map[string]int{
		"users": users, "suspended": suspended, "admins": admins, "proUsers": proUsers,
		"exams": exams, "publishedExams": published, "attempts": attempts,
		"students": students, "classes": classes,
	})
}
func (s *server) adminUsers(w http.ResponseWriter, r *http.Request, id string) {
	rows, err := s.db.Query(r.Context(), `SELECT id,email,full_name,role,suspended_at IS NOT NULL FROM users ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var uid, email, name, role string
		var suspended bool
		if rows.Scan(&uid, &email, &name, &role, &suspended) == nil {
			out = append(out, map[string]any{"id": uid, "email": email, "name": name, "role": role, "suspended": suspended})
		}
	}
	write(w, 200, out)
}
func (s *server) adminAccountStatus(w http.ResponseWriter, r *http.Request, actor string) {
	uid := chi.URLParam(r, "id")
	if uid == actor {
		fail(w, 409, "Cannot change own status")
		return
	}
	var p struct {
		Suspended bool `json:"suspended"`
	}
	if decode(r, &p) != nil {
		fail(w, 400, "Invalid request")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE users SET suspended_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1 AND role='teacher' AND id<>$2`, uid, actor, p.Suspended)
	if err != nil || tag.RowsAffected() == 0 {
		fail(w, 404, "Teacher not found")
		return
	}
	if p.Suspended {
		if _, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, uid); err != nil {
			fail(w, 500, "Unavailable")
			return
		}
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,$2,'user',$3)`, actor, map[bool]string{true: "user.suspended", false: "user.reactivated"}[p.Suspended], uid); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	write(w, 200, map[string]bool{"ok": true})
}
func (s *server) adminAudit(w http.ResponseWriter, r *http.Request, id string) {
	rows, err := s.db.Query(r.Context(), `SELECT action,entity_type,entity_id,created_at FROM audit_logs ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var action, typ string
		var entity any
		var at any
		if rows.Scan(&action, &typ, &entity, &at) == nil {
			out = append(out, map[string]any{"action": action, "entityType": typ, "entityId": entity, "at": at})
		}
	}
	write(w, 200, out)
}
