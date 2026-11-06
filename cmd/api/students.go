package main

import (
	"encoding/csv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *server) students(w http.ResponseWriter, r *http.Request, teacherID string) {
	if r.Method == "GET" {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		classID := r.URL.Query().Get("classId")
		query := `SELECT s.id,s.name,s.email,s.external_id,s.created_at,
			coalesce((SELECT count(*) FROM attempts a WHERE a.student_id=s.id),0)
			FROM students s`
		args := []any{teacherID}
		if classID != "" {
			query += ` JOIN class_members cm ON cm.student_id=s.id AND cm.class_id=$2`
			args = append(args, classID)
		}
		query += ` WHERE s.teacher_id=$1`
		if q != "" {
			args = append(args, "%"+strings.ToLower(q)+"%")
			query += ` AND (lower(s.name) LIKE $` + strconv.Itoa(len(args)) + ` OR lower(s.email) LIKE $` + strconv.Itoa(len(args)) + ` OR lower(s.external_id) LIKE $` + strconv.Itoa(len(args)) + `)`
		}
		query += ` ORDER BY s.created_at DESC LIMIT 500`
		rows, err := s.db.Query(r.Context(), query, args...)
		if err != nil {
			fail(w, 500, "Database error")
			return
		}
		defer rows.Close()
		out := []any{}
		for rows.Next() {
			var id, name, email, external string
			var created time.Time
			var attempts int
			if rows.Scan(&id, &name, &email, &external, &created, &attempts) == nil {
				out = append(out, map[string]any{"id": id, "name": name, "email": email, "externalId": external, "createdAt": created, "attemptCount": attempts})
			}
		}
		write(w, 200, out)
		return
	}
	var p struct{ Name, Email, ExternalID string }
	if decode(r, &p) != nil || strings.TrimSpace(p.Name) == "" || len(p.Name) > 120 || len(p.Email) > 254 || len(p.ExternalID) > 64 {
		fail(w, 400, "Name (max 120 chars) required")
		return
	}
	var existing int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM students WHERE teacher_id=$1`, teacherID).Scan(&existing)
	if !s.requirePlanLimit(w, r, teacherID, "students", freeStudents, existing) {
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `INSERT INTO students(teacher_id,name,email,external_id) VALUES($1,$2,$3,$4) RETURNING id`, teacherID, strings.TrimSpace(p.Name), strings.TrimSpace(p.Email), strings.TrimSpace(p.ExternalID)).Scan(&id)
	if err != nil {
		fail(w, 409, "A student with that name already exists")
		return
	}
	write(w, 201, map[string]string{"id": id})
}

func (s *server) student(w http.ResponseWriter, r *http.Request, teacherID string) {
	sid := chi.URLParam(r, "id")
	switch r.Method {
	case "GET":
		var name, email, external string
		var created time.Time
		err := s.db.QueryRow(r.Context(), `SELECT name,email,external_id,created_at FROM students WHERE id=$1 AND teacher_id=$2`, sid, teacherID).Scan(&name, &email, &external, &created)
		if err != nil {
			fail(w, 404, "Student not found")
			return
		}
		write(w, 200, map[string]any{"id": sid, "name": name, "email": email, "externalId": external, "createdAt": created})
	case "PUT":
		var p struct{ Name, Email, ExternalID string }
		if decode(r, &p) != nil || strings.TrimSpace(p.Name) == "" || len(p.Name) > 120 || len(p.Email) > 254 || len(p.ExternalID) > 64 {
			fail(w, 400, "Name required")
			return
		}
		tag, err := s.db.Exec(r.Context(), `UPDATE students SET name=$3,email=$4,external_id=$5 WHERE id=$1 AND teacher_id=$2`, sid, teacherID, strings.TrimSpace(p.Name), strings.TrimSpace(p.Email), strings.TrimSpace(p.ExternalID))
		if err != nil {
			fail(w, 409, "A student with that name already exists")
			return
		}
		if tag.RowsAffected() == 0 {
			fail(w, 404, "Student not found")
			return
		}
		write(w, 200, map[string]bool{"saved": true})
	case "DELETE":
		tag, err := s.db.Exec(r.Context(), `DELETE FROM students WHERE id=$1 AND teacher_id=$2`, sid, teacherID)
		if err != nil || tag.RowsAffected() == 0 {
			fail(w, 404, "Student not found")
			return
		}
		write(w, 200, map[string]bool{"deleted": true})
	}
}

// studentAttempts returns the exam history for one roster student. Attempts are
// linked by student_id (resolved at start time from the roster name match).
func (s *server) studentAttempts(w http.ResponseWriter, r *http.Request, teacherID string) {
	sid := chi.URLParam(r, "id")
	var owned string
	if err := s.db.QueryRow(r.Context(), `SELECT id FROM students WHERE id=$1 AND teacher_id=$2`, sid, teacherID).Scan(&owned); err != nil {
		fail(w, 404, "Student not found")
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT a.id,e.title,a.started_at,a.submitted_at,a.score,a.total FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.student_id=$1 AND e.teacher_id=$2 ORDER BY a.started_at DESC LIMIT 200`, sid, teacherID)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var aid, title string
		var started time.Time
		var submitted *time.Time
		var score, total *int
		if rows.Scan(&aid, &title, &started, &submitted, &score, &total) == nil {
			out = append(out, map[string]any{"id": aid, "examTitle": title, "startedAt": started, "submittedAt": submitted, "score": score, "total": total})
		}
	}
	write(w, 200, out)
}

// importStudents parses a CSV roster (header: name,email,external_id).
func (s *server) importStudents(w http.ResponseWriter, r *http.Request, teacherID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil || len(body) == 0 {
		fail(w, 400, "CSV body required")
		return
	}
	reader := csv.NewReader(strings.NewReader(string(body)))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil || len(records) < 2 {
		fail(w, 400, "CSV must include a header row and at least one student row")
		return
	}
	header := map[string]int{}
	for i, h := range records[0] {
		header[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	nameCol, ok := header["name"]
	if !ok {
		fail(w, 400, "CSV header must include a 'name' column")
		return
	}
	emailCol, hasEmail := header["email"]
	externalCol, hasExternal := header["external_id"]
	var existing int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM students WHERE teacher_id=$1`, teacherID).Scan(&existing)
	if !s.requirePlanLimit(w, r, teacherID, "students", freeStudents, existing) {
		return
	}
	created := 0
	type skip struct {
		Line   int    `json:"line"`
		Reason string `json:"reason"`
	}
	skipped := []skip{}
	for i, rec := range records[1:] {
		line := i + 2
		get := func(col int, present bool) string {
			if !present || col >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[col])
		}
		name := get(nameCol, true)
		email := get(emailCol, hasEmail)
		external := get(externalCol, hasExternal)
		if name == "" {
			skipped = append(skipped, skip{line, "missing name"})
			continue
		}
		if len(name) > 120 || len(email) > 254 || len(external) > 64 {
			skipped = append(skipped, skip{line, "field too long"})
			continue
		}
		if existing+created >= freeStudents && !s.isPro(r.Context(), teacherID) {
			skipped = append(skipped, skip{line, "plan limit reached"})
			continue
		}
		tag, err := s.db.Exec(r.Context(), `INSERT INTO students(teacher_id,name,email,external_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, teacherID, name, email, external)
		if err != nil {
			skipped = append(skipped, skip{line, "database error"})
			continue
		}
		if tag.RowsAffected() == 0 {
			skipped = append(skipped, skip{line, "duplicate name"})
			continue
		}
		created++
	}
	if created > 0 {
		s.audit(teacherID, "students.imported", "students", "")
	}
	write(w, 200, map[string]any{"created": created, "skipped": skipped})
}

// exportStudents streams the roster as CSV.
func (s *server) exportStudents(w http.ResponseWriter, r *http.Request, teacherID string) {
	rows, err := s.db.Query(r.Context(), `SELECT name,email,external_id FROM students WHERE teacher_id=$1 ORDER BY lower(name)`, teacherID)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="students.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"name", "email", "external_id"})
	for rows.Next() {
		var name, email, external string
		if rows.Scan(&name, &email, &external) == nil {
			_ = cw.Write([]string{name, email, external})
		}
	}
	cw.Flush()
}

func (s *server) classes(w http.ResponseWriter, r *http.Request, teacherID string) {
	if r.Method == "GET" {
		rows, err := s.db.Query(r.Context(), `SELECT c.id,c.name,c.created_at,(SELECT count(*) FROM class_members m WHERE m.class_id=c.id) FROM classes c WHERE c.teacher_id=$1 ORDER BY c.created_at DESC`, teacherID)
		if err != nil {
			fail(w, 500, "Database error")
			return
		}
		defer rows.Close()
		out := []any{}
		for rows.Next() {
			var id, name string
			var created time.Time
			var members int
			if rows.Scan(&id, &name, &created, &members) == nil {
				out = append(out, map[string]any{"id": id, "name": name, "createdAt": created, "memberCount": members})
			}
		}
		write(w, 200, out)
		return
	}
	var p struct{ Name string }
	if decode(r, &p) != nil || strings.TrimSpace(p.Name) == "" || len(p.Name) > 120 {
		fail(w, 400, "Class name required")
		return
	}
	var existing int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM classes WHERE teacher_id=$1`, teacherID).Scan(&existing)
	if !s.requirePlanLimit(w, r, teacherID, "classes", freeClasses, existing) {
		return
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `INSERT INTO classes(teacher_id,name) VALUES($1,$2) RETURNING id`, teacherID, strings.TrimSpace(p.Name)).Scan(&id); err != nil {
		fail(w, 409, "A class with that name already exists")
		return
	}
	write(w, 201, map[string]string{"id": id})
}

func (s *server) class(w http.ResponseWriter, r *http.Request, teacherID string) {
	cid := chi.URLParam(r, "id")
	switch r.Method {
	case "GET":
		var name string
		if err := s.db.QueryRow(r.Context(), `SELECT name FROM classes WHERE id=$1 AND teacher_id=$2`, cid, teacherID).Scan(&name); err != nil {
			fail(w, 404, "Class not found")
			return
		}
		rows, err := s.db.Query(r.Context(), `SELECT s.id,s.name,s.email,s.external_id FROM class_members m JOIN students s ON s.id=m.student_id WHERE m.class_id=$1 ORDER BY lower(s.name)`, cid)
		if err != nil {
			fail(w, 500, "Database error")
			return
		}
		defer rows.Close()
		members := []any{}
		for rows.Next() {
			var id, name, email, external string
			if rows.Scan(&id, &name, &email, &external) == nil {
				members = append(members, map[string]any{"id": id, "name": name, "email": email, "externalId": external})
			}
		}
		write(w, 200, map[string]any{"id": cid, "name": name, "members": members})
	case "PUT":
		var p struct{ Name string }
		if decode(r, &p) != nil || strings.TrimSpace(p.Name) == "" || len(p.Name) > 120 {
			fail(w, 400, "Class name required")
			return
		}
		tag, err := s.db.Exec(r.Context(), `UPDATE classes SET name=$3 WHERE id=$1 AND teacher_id=$2`, cid, teacherID, strings.TrimSpace(p.Name))
		if err != nil {
			fail(w, 409, "A class with that name already exists")
			return
		}
		if tag.RowsAffected() == 0 {
			fail(w, 404, "Class not found")
			return
		}
		write(w, 200, map[string]bool{"saved": true})
	case "DELETE":
		tag, err := s.db.Exec(r.Context(), `DELETE FROM classes WHERE id=$1 AND teacher_id=$2`, cid, teacherID)
		if err != nil || tag.RowsAffected() == 0 {
			fail(w, 404, "Class not found")
			return
		}
		write(w, 200, map[string]bool{"deleted": true})
	}
}

func (s *server) classMembers(w http.ResponseWriter, r *http.Request, teacherID string) {
	cid := chi.URLParam(r, "id")
	var owned string
	if err := s.db.QueryRow(r.Context(), `SELECT id FROM classes WHERE id=$1 AND teacher_id=$2`, cid, teacherID).Scan(&owned); err != nil {
		fail(w, 404, "Class not found")
		return
	}
	if r.Method == "POST" {
		var p struct {
			StudentID string `json:"studentId"`
		}
		if decode(r, &p) != nil || p.StudentID == "" {
			fail(w, 400, "studentId required")
			return
		}
		tag, err := s.db.Exec(r.Context(), `INSERT INTO class_members(class_id,student_id) SELECT $1,s.id FROM students s WHERE s.id=$2 AND s.teacher_id=$3 ON CONFLICT DO NOTHING`, cid, p.StudentID, teacherID)
		if err != nil || tag.RowsAffected() == 0 {
			fail(w, 404, "Student not found")
			return
		}
		write(w, 201, map[string]bool{"added": true})
		return
	}
	sid := chi.URLParam(r, "studentID")
	tag, err := s.db.Exec(r.Context(), `DELETE FROM class_members WHERE class_id=$1 AND student_id=$2`, cid, sid)
	if err != nil || tag.RowsAffected() == 0 {
		fail(w, 404, "Membership not found")
		return
	}
	write(w, 200, map[string]bool{"removed": true})
}

// classReport aggregates performance across a teacher's exams for one class.
func (s *server) classReport(w http.ResponseWriter, r *http.Request, teacherID string) {
	cid := chi.URLParam(r, "id")
	var name string
	if err := s.db.QueryRow(r.Context(), `SELECT name FROM classes WHERE id=$1 AND teacher_id=$2`, cid, teacherID).Scan(&name); err != nil {
		fail(w, 404, "Class not found")
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT s.id,s.name,s.email,s.external_id,
			count(a.id) FILTER (WHERE a.submitted_at IS NOT NULL),
			avg(CASE WHEN a.submitted_at IS NOT NULL AND a.total>0 THEN 100.0*a.score/a.total END),
			max(a.started_at)
		FROM class_members m
		JOIN students s ON s.id=m.student_id AND s.teacher_id=$2
		LEFT JOIN attempts a ON a.student_id=s.id
		LEFT JOIN exams e ON e.id=a.exam_id AND e.teacher_id=$2
		WHERE m.class_id=$1
		GROUP BY s.id,s.name,s.email,s.external_id
		ORDER BY lower(s.name)`, cid, teacherID)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	students := []any{}
	var classAvgSum float64
	var classAvgN int
	for rows.Next() {
		var id, sname, email, external string
		var attempts int
		var avg *float64
		var last *time.Time
		if rows.Scan(&id, &sname, &email, &external, &attempts, &avg, &last) == nil {
			if avg != nil {
				classAvgSum += *avg
				classAvgN++
			}
			students = append(students, map[string]any{"id": id, "name": sname, "email": email, "externalId": external, "completedAttempts": attempts, "avgPercent": avg, "lastAttemptAt": last})
		}
	}
	var overall *float64
	if classAvgN > 0 {
		v := classAvgSum / float64(classAvgN)
		overall = &v
	}
	write(w, 200, map[string]any{"id": cid, "className": name, "studentCount": len(students), "avgPercent": overall, "students": students})
}

// setExamClasses replaces the class assignment of an exam. When at least one
// class is assigned, only roster members of those classes may start attempts.
func (s *server) setExamClasses(w http.ResponseWriter, r *http.Request, teacherID string) {
	eid := chi.URLParam(r, "id")
	var p struct {
		ClassIDs []string `json:"classIds"`
	}
	if decode(r, &p) != nil || len(p.ClassIDs) > 50 {
		fail(w, 400, "Invalid class list")
		return
	}
	seen := map[string]bool{}
	for _, id := range p.ClassIDs {
		if id == "" || seen[id] {
			fail(w, 400, "Duplicate class id")
			return
		}
		seen[id] = true
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer tx.Rollback(r.Context())
	var status string
	if err = tx.QueryRow(r.Context(), `SELECT status FROM exams WHERE id=$1 AND teacher_id=$2 FOR UPDATE`, eid, teacherID).Scan(&status); err != nil {
		fail(w, 404, "Exam not found")
		return
	}
	if status == "archived" {
		fail(w, 409, "Archived exams cannot be assigned")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM exam_classes WHERE exam_id=$1`, eid); err != nil {
		fail(w, 500, "Database error")
		return
	}
	for _, id := range p.ClassIDs {
		tag, err := tx.Exec(r.Context(), `INSERT INTO exam_classes(exam_id,class_id) SELECT $1,c.id FROM classes c WHERE c.id=$2 AND c.teacher_id=$3 ON CONFLICT DO NOTHING`, eid, id, teacherID)
		if err != nil || tag.RowsAffected() == 0 {
			fail(w, 404, "Class not found")
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Database error")
		return
	}
	write(w, 200, map[string]bool{"saved": true})
}

func (s *server) examClasses(w http.ResponseWriter, r *http.Request, teacherID string) {
	eid := chi.URLParam(r, "id")
	var owned string
	if err := s.db.QueryRow(r.Context(), `SELECT id FROM exams WHERE id=$1 AND teacher_id=$2`, eid, teacherID).Scan(&owned); err != nil {
		fail(w, 404, "Exam not found")
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT c.id,c.name FROM exam_classes ec JOIN classes c ON c.id=ec.class_id WHERE ec.exam_id=$1 ORDER BY lower(c.name)`, eid)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out = append(out, map[string]any{"id": id, "name": name})
		}
	}
	write(w, 200, out)
}
