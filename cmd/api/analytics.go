package main

import (
	"net/http"
)

// analytics returns real aggregate performance data for the signed-in teacher.
// Every number is computed from stored attempts — nothing is fabricated.
func (s *server) analytics(w http.ResponseWriter, r *http.Request, teacherID string) {
	var exams, published, attempts, submitted, students int
	err := s.db.QueryRow(r.Context(), `
		SELECT
			(SELECT count(*) FROM exams WHERE teacher_id=$1),
			(SELECT count(*) FROM exams WHERE teacher_id=$1 AND status IN ('published','scheduled')),
			(SELECT count(*) FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE e.teacher_id=$1),
			(SELECT count(*) FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE e.teacher_id=$1 AND a.submitted_at IS NOT NULL),
			(SELECT count(*) FROM students WHERE teacher_id=$1)`, teacherID).
		Scan(&exams, &published, &attempts, &submitted, &students)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}

	// Per-exam performance for submitted attempts.
	rows, err := s.db.Query(r.Context(), `
		SELECT e.id,e.title,e.status,count(a.id),
			avg(CASE WHEN a.submitted_at IS NOT NULL AND a.total>0 THEN 100.0*a.score/a.total END),
			count(a.id) FILTER (WHERE a.submitted_at IS NOT NULL AND a.total>0 AND 100.0*a.score/a.total >= e.passing_percent)
		FROM exams e
		LEFT JOIN attempts a ON a.exam_id=e.id
		WHERE e.teacher_id=$1
		GROUP BY e.id,e.title,e.status
		ORDER BY count(a.id) DESC, e.created_at DESC
		LIMIT 50`, teacherID)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	perExam := []any{}
	for rows.Next() {
		var id, title, status string
		var count int
		var avg *float64
		var passed int
		if rows.Scan(&id, &title, &status, &count, &avg, &passed) == nil {
			perExam = append(perExam, map[string]any{"examId": id, "title": title, "status": status, "attempts": count, "avgPercent": avg, "passedCount": passed})
		}
	}

	// Daily attempt starts for the last 30 days (real activity only).
	days, err := s.db.Query(r.Context(), `
		SELECT to_char(d,'YYYY-MM-DD'),count(a.id)
		FROM generate_series(current_date-29,current_date,interval '1 day') d
		LEFT JOIN attempts a ON a.started_at::date=d::date
		LEFT JOIN exams e ON e.id=a.exam_id AND e.teacher_id=$1
		GROUP BY d ORDER BY d`, teacherID)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer days.Close()
	daily := []any{}
	for days.Next() {
		var day string
		var count int
		if days.Scan(&day, &count) == nil {
			daily = append(daily, map[string]any{"date": day, "attempts": count})
		}
	}

	// Overall averages over submitted attempts.
	var avgPercent *float64
	var passedExams int
	_ = s.db.QueryRow(r.Context(), `
		SELECT avg(100.0*a.score/a.total),
		       count(*) FILTER (WHERE a.total>0 AND 100.0*a.score/a.total >= e.passing_percent)
		FROM attempts a JOIN exams e ON e.id=a.exam_id
		WHERE e.teacher_id=$1 AND a.submitted_at IS NOT NULL AND a.total>0`, teacherID).Scan(&avgPercent, &passedExams)

	passRate := (*float64)(nil)
	if submitted > 0 {
		v := 100.0 * float64(passedExams) / float64(submitted)
		passRate = &v
	}
	write(w, 200, map[string]any{
		"totals": map[string]int{
			"exams": exams, "publishedExams": published, "attempts": attempts,
			"submittedAttempts": submitted, "students": students,
		},
		"avgPercent": avgPercent,
		"passRate":   passRate,
		"perExam":    perExam,
		"daily":      daily,
	})
}
