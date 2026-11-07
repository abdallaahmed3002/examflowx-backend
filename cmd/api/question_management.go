package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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

// bulkQuestions imports multiple questions in a single transaction.
func (s *server) bulkQuestions(w http.ResponseWriter, r *http.Request, teacherID string) {
	eid := chi.URLParam(r, "id")
	var p struct {
		Questions []questionInput `json:"questions"`
	}
	if decode(r, &p) != nil || len(p.Questions) == 0 {
		fail(w, 400, "Questions array required")
		return
	}
	if len(p.Questions) > 100 {
		fail(w, 400, "Maximum 100 questions per bulk import")
		return
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer tx.Rollback(r.Context())

	var currentCount int
	err = tx.QueryRow(r.Context(), `SELECT count(*) FROM questions WHERE exam_id=$1 AND EXISTS(SELECT 1 FROM exams WHERE id=$1 AND teacher_id=$2 AND status='draft')`, eid, teacherID).Scan(&currentCount)
	if err != nil {
		fail(w, 404, "Draft exam not found")
		return
	}

	type validatedQ struct {
		input   questionInput
		correct string
	}
	validated := make([]validatedQ, len(p.Questions))
	for i, q := range p.Questions {
		if q.Points <= 0 {
			q.Points = 1
		}
		// If short question has no expected answer provided, convert to essay to prevent validation error
		if q.Kind == "short" && strings.TrimSpace(q.CorrectAnswer) == "" {
			q.Kind = "essay"
		}
		if q.Kind == "fill" && strings.TrimSpace(q.CorrectAnswer) == "" {
			q.CorrectAnswer = "إجابة نموذجية"
		}
		if err := validateQuestionInput(q); err != nil {
			fail(w, 400, fmt.Sprintf("Question %d: %s", i+1, err.Error()))
			return
		}
		if !json.Valid(q.Options) {
			fail(w, 400, fmt.Sprintf("Question %d: invalid options JSON", i+1))
			return
		}
		correct, err := normalizeCorrect(q, q.Options)
		if err != nil {
			fail(w, 400, fmt.Sprintf("Question %d: invalid correct answer", i+1))
			return
		}
		validated[i] = validatedQ{input: q, correct: correct}
	}

	inserted := 0
	for i, vq := range validated {
		pos := currentCount + i
		_, err := tx.Exec(r.Context(),
			`INSERT INTO questions(exam_id, kind, prompt, options, correct_answer, points, position, image_url) VALUES($1, $2, $3, $4, $5, $6, $7, $8)`,
			eid, vq.input.Kind, vq.input.Prompt, vq.input.Options, vq.correct, vq.input.Points, pos, vq.input.ImageURL)
		if err != nil {
			fail(w, 500, "Failed to save questions")
			return
		}
		inserted++
	}

	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Failed to commit questions")
		return
	}

	write(w, 201, map[string]any{"count": inserted})
}
