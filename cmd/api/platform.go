package main

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Free-plan entitlements. These are enforced server-side on real operations
// (publish, roster, classes) — never in the frontend alone.
const (
	freePublishedExams = 3
	freeStudents       = 50
	freeClasses        = 5
)

type platformState struct {
	RegistrationOpen bool      `json:"registrationOpen"`
	MaintenanceMode  bool      `json:"maintenanceMode"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func (s *server) platform(ctx context.Context) (platformState, error) {
	var st platformState
	err := s.db.QueryRow(ctx, `SELECT registration_open,maintenance_mode,updated_at FROM platform_config WHERE id=1`).Scan(&st.RegistrationOpen, &st.MaintenanceMode, &st.UpdatedAt)
	return st, err
}

// isPro reports whether the account currently holds an active pro plan.
func (s *server) isPro(ctx context.Context, userID string) bool {
	var pro bool
	_ = s.db.QueryRow(ctx, `SELECT plan='pro' AND (plan_expires_at IS NULL OR plan_expires_at>now()) FROM users WHERE id=$1`, userID).Scan(&pro)
	return pro
}

func (s *server) adminPlatform(w http.ResponseWriter, r *http.Request, adminID string) {
	if r.Method == "GET" {
		st, err := s.platform(r.Context())
		if err != nil {
			fail(w, 500, "Unavailable")
			return
		}
		write(w, 200, map[string]any{
			"registrationOpen": st.RegistrationOpen,
			"maintenanceMode":  st.MaintenanceMode,
			"updatedAt":        st.UpdatedAt,
			"freeLimits": map[string]int{
				"publishedExams": freePublishedExams,
				"students":       freeStudents,
				"classes":        freeClasses,
			},
		})
		return
	}
	var p struct {
		RegistrationOpen *bool `json:"registrationOpen"`
		MaintenanceMode  *bool `json:"maintenanceMode"`
	}
	if decode(r, &p) != nil || (p.RegistrationOpen == nil && p.MaintenanceMode == nil) {
		fail(w, 400, "registrationOpen and/or maintenanceMode required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var reg, maint bool
	err = tx.QueryRow(r.Context(), `UPDATE platform_config SET
		registration_open=COALESCE($1,registration_open),
		maintenance_mode=COALESCE($2,maintenance_mode),
		updated_at=now() WHERE id=1 RETURNING registration_open,maintenance_mode`,
		p.RegistrationOpen, p.MaintenanceMode).Scan(&reg, &maint)
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,'platform.updated','platform',NULL)`, adminID); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	write(w, 200, map[string]any{"registrationOpen": reg, "maintenanceMode": maint})
}

// adminUserRole changes a user's role (teacher <-> admin). An admin can never
// change their own role, so the platform always keeps at least one admin.
func (s *server) adminUserRole(w http.ResponseWriter, r *http.Request, adminID string) {
	uid := chi.URLParam(r, "id")
	var p struct {
		Role string `json:"role"`
	}
	if decode(r, &p) != nil || (p.Role != "teacher" && p.Role != "admin") {
		fail(w, 400, "role must be 'teacher' or 'admin'")
		return
	}
	if uid == adminID {
		fail(w, 409, "Cannot change your own role")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE users SET role=$3,suspended_at=NULL WHERE id=$1 AND id<>$2 AND role IN ('teacher','admin')`, uid, adminID, p.Role)
	if err != nil || tag.RowsAffected() == 0 {
		fail(w, 404, "User not found")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,$2,'user',$3)`, adminID, "admin.role_"+p.Role, uid); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	write(w, 200, map[string]any{"ok": true, "role": p.Role})
}

// adminUserPlan grants or revokes a plan entitlement. Expiry in the past
// means the entitlement is already inactive (used to test/roll back grants).
func (s *server) adminUserPlan(w http.ResponseWriter, r *http.Request, adminID string) {
	uid := chi.URLParam(r, "id")
	var p struct {
		Plan      string     `json:"plan"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if decode(r, &p) != nil || (p.Plan != "free" && p.Plan != "pro") {
		fail(w, 400, "plan must be 'free' or 'pro'")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE users SET plan=$3,plan_expires_at=$4,plan_updated_at=now() WHERE id=$1 AND id<>$2`, uid, adminID, p.Plan, p.ExpiresAt)
	if err != nil || tag.RowsAffected() == 0 {
		fail(w, 404, "User not found")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor_id,action,entity_type,entity_id) VALUES($1,$2,'user',$3)`, adminID, "admin.plan_"+p.Plan, uid); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unavailable")
		return
	}
	write(w, 200, map[string]any{"ok": true, "plan": p.Plan, "expiresAt": p.ExpiresAt})
}

// requirePlanLimit rejects free-plan operations beyond entitlements with 402.
func (s *server) requirePlanLimit(w http.ResponseWriter, r *http.Request, userID, resource string, limit int, count int) bool {
	if s.isPro(r.Context(), userID) || count < limit {
		return true
	}
	write(w, 402, map[string]any{
		"error":    "Free plan limit reached: " + resource + " (limit " + itoaStr(limit) + "). Ask an administrator to upgrade your plan.",
		"code":     "plan_limit",
		"resource": resource,
		"limit":    limit,
	})
	return false
}

func itoaStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
