package main

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"strings"
	"time"
)

type examSettings struct {
	Title            string     `json:"title"`
	Subject          string     `json:"subject"`
	Description      string     `json:"description"`
	Instructions     string     `json:"instructions"`
	Duration         int        `json:"duration"`
	PassingPercent   int        `json:"passingPercent"`
	MaxAttempts      int        `json:"maxAttempts"`
	ResultVisibility string     `json:"resultVisibility"`
	StartsAt         *time.Time `json:"startsAt"`
	EndsAt           *time.Time `json:"endsAt"`
}

func validSettings(p examSettings) bool {
	return strings.TrimSpace(p.Title) != "" && len(p.Title) <= 200 && len(p.Description) <= 10000 && len(p.Instructions) <= 10000 && p.Duration >= 1 && p.Duration <= 480 && p.PassingPercent >= 0 && p.PassingPercent <= 100 && p.MaxAttempts >= 1 && p.MaxAttempts <= 20 && (p.ResultVisibility == "immediate" || p.ResultVisibility == "teacher_only") && (p.StartsAt == nil || p.EndsAt == nil || p.EndsAt.After(*p.StartsAt))
}
func (s *server) updateExam(w http.ResponseWriter, r *http.Request, teacherID string) {
	var p examSettings
	if decode(r, &p) != nil || !validSettings(p) {
		fail(w, 400, "Invalid exam settings")
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE exams SET title=$3,subject=$4,description=$5,instructions=$6,duration_minutes=$7,passing_percent=$8,max_attempts=$9,starts_at=$10,ends_at=$11,result_visibility=$12,updated_at=now() WHERE id=$1 AND teacher_id=$2 AND status='draft'`, chi.URLParam(r, "id"), teacherID, p.Title, p.Subject, p.Description, p.Instructions, p.Duration, p.PassingPercent, p.MaxAttempts, p.StartsAt, p.EndsAt, p.ResultVisibility)
	if err != nil {
		fail(w, 500, "Unable to update exam")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 404, "Draft exam not found")
		return
	}
	write(w, 200, map[string]bool{"saved": true})
}
func (s *server) changeStatus(w http.ResponseWriter, r *http.Request, teacherID string) {
	action := chi.URLParam(r, "action")
	transitions := map[string][]string{"close": {"published", "scheduled"}, "archive": {"draft", "closed"}, "unpublish": {"published", "scheduled"}}
	from, ok := transitions[action]
	if !ok {
		fail(w, 400, "Unknown action")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unable to change exam status")
		return
	}
	defer tx.Rollback(r.Context())
	var examID string
	// Unpublishing is safe even with attempts: every attempt reads its own
	// immutable question snapshot, so later edits cannot alter them.
	err = tx.QueryRow(r.Context(), `UPDATE exams SET status=CASE WHEN $3='unpublish' THEN 'draft' WHEN $3='archive' THEN 'archived' ELSE 'closed' END,updated_at=now() WHERE id=$1 AND teacher_id=$2 AND status=ANY($4) RETURNING id`, chi.URLParam(r, "id"), teacherID, action, from).Scan(&examID)
	if err != nil {
		fail(w, 409, "Transition unavailable")
		return
	}
	if action == "close" {
		// Closing an exam finalizes every still-active attempt so students
		// are graded immediately instead of waiting for background expiry.
		rows, qerr := tx.Query(r.Context(), `SELECT id FROM attempts WHERE exam_id=$1 AND submitted_at IS NULL`, examID)
		if qerr != nil {
			fail(w, 500, "Unable to close exam")
			return
		}
		var activeIDs []string
		for rows.Next() {
			var aid string
			if rows.Scan(&aid) == nil {
				activeIDs = append(activeIDs, aid)
			}
		}
		rows.Close()
		for _, aid := range activeIDs {
			var score, total int
			if err = tx.QueryRow(r.Context(), gradeSQL, aid).Scan(&score, &total); err != nil {
				fail(w, 500, "Unable to close exam")
				return
			}
			if _, err = tx.Exec(r.Context(), `UPDATE attempts SET submitted_at=now(),score=$2,total=$3 WHERE id=$1 AND submitted_at IS NULL`, aid, score, total); err != nil {
				fail(w, 500, "Unable to close exam")
				return
			}
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unable to change exam status")
		return
	}
	s.audit(teacherID, "exam."+action, "exam", examID)
	write(w, 200, map[string]bool{"ok": true})
}

// setResultVisibility lets the teacher release or hide scores at any point in
// the exam lifecycle ("release results"), without touching attempt data.
func (s *server) setResultVisibility(w http.ResponseWriter, r *http.Request, teacherID string) {
	var p struct {
		Visibility string `json:"visibility"`
	}
	if decode(r, &p) != nil || (p.Visibility != "immediate" && p.Visibility != "teacher_only") {
		fail(w, 400, "visibility must be 'immediate' or 'teacher_only'")
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE exams SET result_visibility=$3,updated_at=now() WHERE id=$1 AND teacher_id=$2 AND status!='archived'`, chi.URLParam(r, "id"), teacherID, p.Visibility)
	if err != nil {
		fail(w, 500, "Unable to update result visibility")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 404, "Exam not found")
		return
	}
	s.audit(teacherID, "exam.results_"+p.Visibility, "exam", chi.URLParam(r, "id"))
	write(w, 200, map[string]bool{"saved": true})
}

func (s *server) duplicateExam(w http.ResponseWriter, r *http.Request, teacherID string) {
	id := chi.URLParam(r, "id")
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unable to duplicate exam")
		return
	}
	defer tx.Rollback(r.Context())
	var newID string
	err = tx.QueryRow(r.Context(), `INSERT INTO exams(teacher_id,public_token,title,subject,description,instructions,duration_minutes,passing_percent,max_attempts,starts_at,ends_at,result_visibility) SELECT teacher_id,$3,title || ' (copy)',subject,description,instructions,duration_minutes,passing_percent,max_attempts,starts_at,ends_at,result_visibility FROM exams WHERE id=$1 AND teacher_id=$2 AND status!='archived' RETURNING id`, id, teacherID, token()).Scan(&newID)
	if err != nil {
		fail(w, 404, "Exam not found")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO questions(exam_id,kind,prompt,options,correct_answer,points,position,image_url) SELECT $1,kind,prompt,options,correct_answer,points,position,image_url FROM questions WHERE exam_id=$2`, newID, id)
	if err != nil {
		fail(w, 500, "Unable to duplicate exam")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unable to duplicate exam")
		return
	}
	write(w, 201, map[string]string{"id": newID})
}
func (s *server) deleteExam(w http.ResponseWriter, r *http.Request, teacherID string) {
	tag, err := s.db.Exec(r.Context(), `DELETE FROM exams WHERE id=$1 AND teacher_id=$2 AND status='draft' AND NOT EXISTS(SELECT 1 FROM attempts a WHERE a.exam_id=exams.id)`, chi.URLParam(r, "id"), teacherID)
	if err != nil {
		fail(w, 500, "Unable to delete exam")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 409, "Only unused drafts can be deleted")
		return
	}
	s.audit(teacherID, "exam.deleted", "exam", chi.URLParam(r, "id"))
	write(w, 200, map[string]bool{"deleted": true})
}
