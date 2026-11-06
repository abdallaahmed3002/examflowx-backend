package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type questionInput struct {
	Kind          string          `json:"kind"`
	Prompt        string          `json:"prompt"`
	CorrectAnswer string          `json:"correctAnswer"`
	Options       json.RawMessage `json:"options"`
	ImageURL      string          `json:"imageUrl"`
	Points        int             `json:"points"`
}

func (s *server) editQuestion(w http.ResponseWriter, r *http.Request, teacherID string) {
	eid, qid := chi.URLParam(r, "id"), chi.URLParam(r, "questionID")
	if r.Method == "DELETE" {
		// Questions already captured in an attempt snapshot must stay so
		// historical attempts remain auditable and gradeable.
		var referenced bool
		if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM attempt_questions WHERE question_id=$1)`, qid).Scan(&referenced); err != nil {
			fail(w, 500, "Unable to delete question")
			return
		}
		if referenced {
			fail(w, 409, "Question belongs to an existing attempt and cannot be deleted")
			return
		}
		tag, err := s.db.Exec(r.Context(), `DELETE FROM questions q USING exams e WHERE q.id=$1 AND q.exam_id=$2 AND e.id=q.exam_id AND e.teacher_id=$3 AND e.status='draft'`, qid, eid, teacherID)
		if err != nil {
			fail(w, 500, "Unable to delete question")
			return
		}
		if tag.RowsAffected() == 0 {
			fail(w, 404, "Draft question not found")
			return
		}
		write(w, 200, map[string]bool{"deleted": true})
		return
	}
	var p questionInput
	if decode(r, &p) != nil || !json.Valid(p.Options) {
		fail(w, 400, "Invalid question")
		return
	}
	if err := validateQuestionInput(p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	correct, err := normalizeCorrect(p, p.Options)
	if err != nil {
		fail(w, 400, "Invalid correct answer")
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE questions q SET kind=$4,prompt=$5,options=$6,correct_answer=$7,points=$8,image_url=$9 FROM exams e WHERE q.id=$1 AND q.exam_id=$2 AND e.id=q.exam_id AND e.teacher_id=$3 AND e.status='draft'`, qid, eid, teacherID, p.Kind, p.Prompt, p.Options, correct, p.Points, p.ImageURL)
	if err != nil {
		fail(w, 500, "Unable to update question")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 404, "Draft question not found")
		return
	}
	write(w, 200, map[string]bool{"saved": true})
}
func (s *server) duplicateQuestion(w http.ResponseWriter, r *http.Request, teacherID string) {
	eid, qid := chi.URLParam(r, "id"), chi.URLParam(r, "questionID")
	var newID string
	err := s.db.QueryRow(r.Context(), `INSERT INTO questions(exam_id,kind,prompt,options,correct_answer,points,position,image_url) SELECT q.exam_id,q.kind,q.prompt,q.options,q.correct_answer,q.points,(SELECT coalesce(max(position),0)+1 FROM questions WHERE exam_id=$1),q.image_url FROM questions q JOIN exams e ON e.id=q.exam_id WHERE q.id=$2 AND q.exam_id=$1 AND e.teacher_id=$3 AND e.status='draft' RETURNING id`, eid, qid, teacherID).Scan(&newID)
	if err != nil {
		fail(w, 404, "Draft question not found")
		return
	}
	write(w, 201, map[string]string{"id": newID})
}
func (s *server) reorderQuestions(w http.ResponseWriter, r *http.Request, teacherID string) {
	var p struct {
		IDs []string `json:"ids"`
	}
	if decode(r, &p) != nil || len(p.IDs) == 0 || len(p.IDs) > 500 {
		fail(w, 400, "Invalid question order")
		return
	}
	seen := map[string]bool{}
	for _, id := range p.IDs {
		if seen[id] {
			fail(w, 400, "Duplicate question ID")
			return
		}
		seen[id] = true
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Unable to reorder")
		return
	}
	defer tx.Rollback(r.Context())
	var count int
	var locked string
	err = tx.QueryRow(r.Context(), `SELECT id FROM exams WHERE id=$1 AND teacher_id=$2 AND status='draft' FOR UPDATE`, chi.URLParam(r, "id"), teacherID).Scan(&locked)
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT count(*) FROM questions WHERE exam_id=$1`, locked).Scan(&count)
	}
	if err != nil || count != len(p.IDs) {
		fail(w, 409, "Question list changed or exam unavailable")
		return
	}
	for i, id := range p.IDs {
		tag, err := tx.Exec(r.Context(), `UPDATE questions SET position=$3 WHERE id=$1 AND exam_id=$2`, id, chi.URLParam(r, "id"), i)
		if err != nil || tag.RowsAffected() != 1 {
			fail(w, 409, "Question list changed")
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Unable to reorder")
		return
	}
	write(w, 200, map[string]bool{"saved": true})
}
