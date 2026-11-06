package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"examflow/backend/internal/security"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type server struct{ db *pgxpool.Pool }

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// dummyHash equalizes login timing for unknown accounts.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("examflow-timing-pad"), bcrypt.DefaultCost)

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	write(w, code, map[string]string{"error": msg})
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}
func (s *server) user(r *http.Request) (string, error) {
	c, e := r.Cookie("examflow_session")
	if e != nil {
		return "", e
	}
	var id string
	e = s.db.QueryRow(r.Context(), `SELECT user_id FROM sessions WHERE token_hash=$1 AND expires_at>now() AND EXISTS(SELECT 1 FROM users u WHERE u.id=sessions.user_id AND u.suspended_at IS NULL)`, hash(c.Value)).Scan(&id)
	return id, e
}
func (s *server) protected(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, e := s.user(r)
		if e != nil {
			fail(w, 401, "Sign in required")
			return
		}
		fn(w, r, id)
	}
}
func (s *server) auth(w http.ResponseWriter, r *http.Request) {
	var p struct{ Email, Password, FullName string }
	if decode(r, &p) != nil {
		fail(w, 400, "Invalid request")
		return
	}
	p.Email = strings.ToLower(strings.TrimSpace(p.Email))
	if len(p.Email) < 5 || len(p.Password) < 10 {
		fail(w, 400, "Valid email and password of at least 10 characters required")
		return
	}
	var id, name string
	var hashed string
	if strings.HasSuffix(r.URL.Path, "register") {
		if strings.TrimSpace(p.FullName) == "" {
			fail(w, 400, "Name required")
			return
		}
		if st, err := s.platform(r.Context()); err != nil {
			fail(w, 503, "Registration unavailable")
			return
		} else if !st.RegistrationOpen {
			write(w, 403, map[string]string{"error": "Registration is currently closed", "code": "registration_closed"})
			return
		}
		b, _ := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
		e := s.db.QueryRow(r.Context(), `INSERT INTO users(email,password_hash,full_name) VALUES($1,$2,$3) RETURNING id`, p.Email, string(b), p.FullName).Scan(&id)
		if e != nil {
			fail(w, 409, "Unable to register email")
			return
		}
		name = p.FullName
	} else {
		e := s.db.QueryRow(r.Context(), `SELECT id,password_hash,full_name FROM users WHERE email=$1 AND suspended_at IS NULL`, p.Email).Scan(&id, &hashed, &name)
		if e != nil {
			// Equalize timing so unknown emails are not distinguishable.
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))
			fail(w, 401, "Invalid credentials")
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(hashed), []byte(p.Password)) != nil {
			fail(w, 401, "Invalid credentials")
			return
		}
	}
	t := token()
	_, e := s.db.Exec(r.Context(), `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '7 days')`, hash(t), id)
	if e != nil {
		fail(w, 500, "Session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "examflow_session", Value: t, Path: "/", HttpOnly: true, Secure: os.Getenv("COOKIE_SECURE") == "true", SameSite: http.SameSiteLaxMode, MaxAge: 604800})
	s.audit(id, "auth.login", "user", id)
	write(w, 200, map[string]string{"id": id, "name": name})
}
func (s *server) me(w http.ResponseWriter, r *http.Request, id string) {
	var name, email, role string
	_ = s.db.QueryRow(r.Context(), `SELECT full_name,email,role FROM users WHERE id=$1`, id).Scan(&name, &email, &role)
	write(w, 200, map[string]string{"id": id, "name": name, "email": email, "role": role})
}
func (s *server) logout(w http.ResponseWriter, r *http.Request, id string) {
	if c, e := r.Cookie("examflow_session"); e == nil {
		_, _ = s.db.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "examflow_session", Path: "/", MaxAge: -1, HttpOnly: true})
	s.audit(id, "auth.logout", "user", id)
	write(w, 200, map[string]bool{"ok": true})
}
func (s *server) exams(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == "GET" {
		rows, e := s.db.Query(r.Context(), `SELECT e.id,e.title,e.subject,e.duration_minutes,e.status,e.public_token,e.starts_at,(SELECT count(*) FROM questions q WHERE q.exam_id=e.id),(SELECT count(*) FROM attempts a WHERE a.exam_id=e.id) FROM exams e WHERE teacher_id=$1 ORDER BY created_at DESC`, id)
		if e != nil {
			fail(w, 500, "Database error")
			return
		}
		defer rows.Close()
		out := []any{}
		for rows.Next() {
			var eid, title, subject, status, pub string
			var duration, q, a int
			var starts *time.Time
			if err := rows.Scan(&eid, &title, &subject, &duration, &status, &pub, &starts, &q, &a); err != nil {
				fail(w, 500, "Database error")
				return
			}
			out = append(out, map[string]any{"id": eid, "title": title, "subject": subject, "duration": duration, "status": status, "publicToken": pub, "questionCount": q, "attemptCount": a, "startsAt": starts})
		}
		write(w, 200, out)
		return
	}
	var p struct {
		Title, Subject, Description string
		Duration                    int
	}
	if decode(r, &p) != nil || strings.TrimSpace(p.Title) == "" || p.Duration < 1 || p.Duration > 480 {
		fail(w, 400, "Title and duration (1–480 minutes) required")
		return
	}
	var eid string
	e := s.db.QueryRow(r.Context(), `INSERT INTO exams(teacher_id,public_token,title,subject,description,duration_minutes) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, id, token(), p.Title, p.Subject, p.Description, p.Duration).Scan(&eid)
	if e != nil {
		fail(w, 500, "Database error")
		return
	}
	s.audit(id, "exam.created", "exam", eid)
	write(w, 201, map[string]string{"id": eid})
}
func (s *server) exam(w http.ResponseWriter, r *http.Request, id string) {
	eid := chi.URLParam(r, "id")
	var title, subject, description, instructions, status, pub string
	var duration, passing, maxAttempts int
	var visibility string
	var starts, ends *time.Time
	e := s.db.QueryRow(r.Context(), `SELECT title,subject,description,instructions,duration_minutes,passing_percent,max_attempts,starts_at,ends_at,result_visibility,status,public_token FROM exams WHERE id=$1 AND teacher_id=$2`, eid, id).Scan(&title, &subject, &description, &instructions, &duration, &passing, &maxAttempts, &starts, &ends, &visibility, &status, &pub)
	if e != nil {
		fail(w, 404, "Exam not found")
		return
	}
	if r.Method == "POST" {
		if status != "draft" {
			fail(w, 409, "Only drafts can be published")
			return
		}
		var count int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM questions WHERE exam_id=$1`, eid).Scan(&count)
		if count == 0 {
			fail(w, 400, "Add a question before publishing")
			return
		}
		// Entitlement: free plans may keep a bounded number of live exams.
		var live int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM exams WHERE teacher_id=$1 AND status IN ('published','scheduled')`, id).Scan(&live)
		if !s.isPro(r.Context(), id) && live >= freePublishedExams {
			write(w, 402, map[string]any{"error": "Free plan limit: " + itoaStr(freePublishedExams) + " published exams. Ask an administrator to upgrade your plan.", "code": "plan_limit", "resource": "publishedExams", "limit": freePublishedExams})
			return
		}
		target := "published"
		if starts != nil && starts.After(time.Now()) {
			target = "scheduled"
		}
		tag, updateErr := s.db.Exec(r.Context(), `UPDATE exams SET status=$3,updated_at=now() WHERE id=$1 AND teacher_id=$2 AND status='draft' AND EXISTS(SELECT 1 FROM questions WHERE exam_id=$1)`, eid, id, target)
		e = updateErr
		if e == nil && tag.RowsAffected() == 0 {
			fail(w, 409, "Exam was changed concurrently")
			return
		}
		if e != nil {
			fail(w, 500, "Database error")
			return
		}
		status = target
		s.audit(id, "exam.published", "exam", eid)
	}
	rows, e := s.db.Query(r.Context(), `SELECT id,kind,prompt,options,correct_answer,points,image_url FROM questions WHERE exam_id=$1 ORDER BY position,id`, eid)
	if e != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	questions := []any{}
	for rows.Next() {
		var qid, kind, prompt, correct, imageURL string
		var opts json.RawMessage
		var points int
		_ = rows.Scan(&qid, &kind, &prompt, &opts, &correct, &points, &imageURL)
		questions = append(questions, map[string]any{"id": qid, "kind": kind, "prompt": prompt, "options": opts, "correctAnswer": correct, "points": points, "imageUrl": imageURL})
	}
	write(w, 200, map[string]any{"id": eid, "title": title, "subject": subject, "description": description, "instructions": instructions, "passingPercent": passing, "maxAttempts": maxAttempts, "resultVisibility": visibility, "startsAt": starts, "endsAt": ends, "duration": duration, "status": status, "publicToken": pub, "questions": questions})
}
func (s *server) question(w http.ResponseWriter, r *http.Request, id string) {
	eid := chi.URLParam(r, "id")
	var p questionInput
	if decode(r, &p) != nil {
		fail(w, 400, "Invalid question")
		return
	}
	if err := validateQuestionInput(p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if !json.Valid(p.Options) {
		fail(w, 400, "Invalid options")
		return
	}
	correct, err := normalizeCorrect(p, p.Options)
	if err != nil {
		fail(w, 400, "Invalid correct answer")
		return
	}
	var qid string
	e := s.db.QueryRow(r.Context(), `INSERT INTO questions(exam_id,kind,prompt,options,correct_answer,points,position,image_url) SELECT id,$3,$4,$5,$6,$7,(SELECT count(*) FROM questions WHERE exam_id=$1),$8 FROM exams WHERE id=$1 AND teacher_id=$2 AND status='draft' RETURNING id`, eid, id, p.Kind, p.Prompt, p.Options, correct, p.Points, p.ImageURL).Scan(&qid)
	if e != nil {
		fail(w, 404, "Draft exam not found")
		return
	}
	write(w, 201, map[string]string{"id": qid})
}
func (s *server) public(w http.ResponseWriter, r *http.Request) {
	pub := chi.URLParam(r, "token")
	var title, subject, description, instructions, name string
	var duration int
	e := s.db.QueryRow(r.Context(), `SELECT e.title,e.subject,e.description,e.instructions,e.duration_minutes,u.full_name FROM exams e JOIN users u ON u.id=e.teacher_id WHERE e.public_token=$1 AND e.status IN ('published','scheduled') AND (e.starts_at IS NULL OR e.starts_at<=now()) AND (e.ends_at IS NULL OR e.ends_at>now())`, pub).Scan(&title, &subject, &description, &instructions, &duration, &name)
	if e != nil {
		fail(w, 404, "Exam unavailable")
		return
	}
	write(w, 200, map[string]any{"title": title, "subject": subject, "description": description, "instructions": instructions, "duration": duration, "teacher": name})
}
func (s *server) start(w http.ResponseWriter, r *http.Request) {
	var p struct{ StudentName string }
	if decode(r, &p) != nil || len(strings.TrimSpace(p.StudentName)) < 2 {
		fail(w, 400, "Student name required")
		return
	}
	if st, err := s.platform(r.Context()); err != nil {
		fail(w, 503, "Unavailable")
		return
	} else if st.MaintenanceMode {
		write(w, 503, map[string]string{"error": "The platform is in maintenance. Please try again later.", "code": "maintenance"})
		return
	}
	pub := chi.URLParam(r, "token")
	t := token()
	name := strings.TrimSpace(p.StudentName)
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		fail(w, 500, "Database error")
		return
	}
	defer tx.Rollback(r.Context())
	// Lock the exam row so concurrent starts cannot exceed max_attempts.
	var eid, teacherID string
	var duration, assigned int
	var ends *time.Time
	e = tx.QueryRow(r.Context(), `SELECT id,teacher_id,duration_minutes,ends_at,(SELECT count(*) FROM exam_classes ec WHERE ec.exam_id=exams.id) FROM exams WHERE public_token=$1 AND status IN ('published','scheduled') AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now()) FOR UPDATE`, pub).Scan(&eid, &teacherID, &duration, &ends, &assigned)
	if e != nil {
		fail(w, 404, "Exam unavailable")
		return
	}
	var maxAttempts, used int
	if e = tx.QueryRow(r.Context(), `SELECT max_attempts,(SELECT count(*) FROM attempts a WHERE a.exam_id=$1 AND lower(a.student_name)=lower($2)) FROM exams WHERE id=$1`, eid, name).Scan(&maxAttempts, &used); e != nil {
		fail(w, 500, "Database error")
		return
	}
	if used >= maxAttempts {
		fail(w, 409, "Attempt limit reached for this name")
		return
	}
	// Resolve the roster identity and enforce group-based assignment: when the
	// exam is assigned to classes, only members of those classes may start.
	var studentID *string
	_ = tx.QueryRow(r.Context(), `SELECT id FROM students WHERE teacher_id=$1 AND lower(name)=lower($2)`, teacherID, name).Scan(&studentID)
	if assigned > 0 {
		if studentID == nil {
			write(w, 403, map[string]string{"error": "This exam is restricted to an assigned class. Use the exact name your teacher has on file.", "code": "not_assigned"})
			return
		}
		var member bool
		if e = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM exam_classes ec JOIN class_members m ON m.class_id=ec.class_id WHERE ec.exam_id=$1 AND m.student_id=$2)`, eid, *studentID).Scan(&member); e != nil {
			fail(w, 500, "Database error")
			return
		}
		if !member {
			write(w, 403, map[string]string{"error": "This exam is restricted to an assigned class. Use the exact name your teacher has on file.", "code": "not_assigned"})
			return
		}
	}
	expires := time.Now().Add(time.Duration(duration) * time.Minute)
	if ends != nil && ends.Before(expires) {
		expires = *ends
	}
	var id string
	e = tx.QueryRow(r.Context(), `INSERT INTO attempts(attempt_token,exam_id,student_name,expires_at,student_id) VALUES($1,$2,$3,$4,$5) RETURNING id`, t, eid, name, expires, studentID).Scan(&id)
	if e != nil {
		fail(w, 500, "Unable to start attempt")
		return
	}
	// Freeze the question set for this attempt: later exam edits must never
	// alter what this student sees or how it is graded.
	if _, e = tx.Exec(r.Context(), `INSERT INTO attempt_questions(attempt_id,question_id,kind,prompt,options,correct_answer,image_url,points,position) SELECT $1,id,kind,prompt,options,correct_answer,image_url,points,position FROM questions WHERE exam_id=$2`, id, eid); e != nil {
		fail(w, 500, "Unable to start attempt")
		return
	}
	var count int
	if e = tx.QueryRow(r.Context(), `SELECT count(*) FROM attempt_questions WHERE attempt_id=$1`, id).Scan(&count); e != nil || count == 0 {
		fail(w, 409, "Exam has no questions")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "Unable to start attempt")
		return
	}
	write(w, 201, map[string]string{"token": t})
}
func (s *server) finalize(ctx context.Context, t string) error {
	tx, e := s.db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var id string
	var submitted *time.Time
	e = tx.QueryRow(ctx, `SELECT id,submitted_at FROM attempts WHERE attempt_token=$1 FOR UPDATE`, t).Scan(&id, &submitted)
	if e != nil {
		return e
	}
	if submitted != nil {
		// Already finalized: idempotent success for duplicate submissions.
		return tx.Commit(ctx)
	}
	var score, total int
	e = tx.QueryRow(ctx, gradeSQL, id).Scan(&score, &total)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE attempts SET submitted_at=now(),score=$2,total=$3 WHERE id=$1`, id, score, total)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// shuffleOptions deterministically reorders ordering-question items per
// attempt and question: the teacher stores items in correct order, but the
// student always sees the same shuffled order across reloads without the
// answer leaking.
func shuffleOptions(attemptToken, questionID string, raw json.RawMessage) json.RawMessage {
	var items []string
	if json.Unmarshal(raw, &items) != nil || len(items) < 2 {
		return raw
	}
	key := attemptToken + "|" + questionID
	rank := func(s string) string {
		sum := md5.Sum([]byte(key + "|" + s))
		return hex.EncodeToString(sum[:])
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := rank(items[i]), rank(items[j])
		if ri != rj {
			return ri < rj
		}
		return items[i] < items[j]
	})
	out, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	return out
}

func (s *server) attempt(w http.ResponseWriter, r *http.Request) {
	t := chi.URLParam(r, "token")
	var id, title, name, visibility string
	var expires time.Time
	var submitted *time.Time
	var score, total *int
	var revision int64
	e := s.db.QueryRow(r.Context(), `SELECT a.id,e.title,a.student_name,LEAST(a.expires_at,COALESCE(e.ends_at,'infinity'::timestamptz)),a.submitted_at,a.score,a.total,e.result_visibility,a.answer_revision FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.attempt_token=$1`, t).Scan(&id, &title, &name, &expires, &submitted, &score, &total, &visibility, &revision)
	if e != nil {
		fail(w, 404, "Attempt not found")
		return
	}
	if submitted == nil && time.Now().After(expires) {
		if err := s.finalize(r.Context(), t); err != nil {
			fail(w, 503, "Submission unavailable")
			return
		}
		http.Redirect(w, r, "/api/v1/attempts/"+t, http.StatusTemporaryRedirect)
		return
	}
	if submitted != nil {
		result := map[string]any{"submitted": true, "title": title, "studentName": name, "resultVisible": visibility == "immediate"}
		if visibility == "immediate" {
			result["score"] = score
			result["total"] = total
		}
		write(w, 200, result)
		return
	}
	// Serve exclusively from the per-attempt snapshot so teacher edits to the
	// exam can never change this attempt's questions mid-flight.
	rows, e := s.db.Query(r.Context(), `SELECT aq.question_id,aq.kind,aq.prompt,aq.options,aq.points,aq.image_url,coalesce(ans.value,'') FROM attempt_questions aq LEFT JOIN answers ans ON ans.attempt_id=aq.attempt_id AND ans.question_id=aq.question_id WHERE aq.attempt_id=$1 ORDER BY aq.position,aq.question_id`, id)
	if e != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	qs := []any{}
	for rows.Next() {
		var qid, kind, prompt, value, imageURL string
		var opts json.RawMessage
		var points int
		_ = rows.Scan(&qid, &kind, &prompt, &opts, &points, &imageURL, &value)
		if kind == "ordering" {
			opts = shuffleOptions(t, qid, opts)
		}
		qs = append(qs, map[string]any{"id": qid, "kind": kind, "prompt": prompt, "options": opts, "points": points, "value": value, "imageUrl": imageURL})
	}
	write(w, 200, map[string]any{"title": title, "studentName": name, "expiresAt": expires, "serverNow": time.Now().UTC(), "revision": revision, "submitted": false, "questions": qs})
}
func (s *server) answer(w http.ResponseWriter, r *http.Request) {
	t := chi.URLParam(r, "token")
	var p struct {
		QuestionID   string `json:"questionID"`
		Value        string `json:"value"`
		BaseRevision *int64 `json:"baseRevision"`
	}
	if decode(r, &p) != nil || p.QuestionID == "" {
		fail(w, 400, "Invalid answer")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer tx.Rollback(r.Context())
	// Lock the attempt before validating status: finalization locks this same
	// row, so an in-flight answer can never be written after grading commits.
	// The effective deadline also honors a teacher shortening exam ends_at.
	var id string
	var revision int64
	var expires time.Time
	err = tx.QueryRow(r.Context(), `SELECT a.id,a.answer_revision,LEAST(a.expires_at,COALESCE(e.ends_at,'infinity'::timestamptz)) FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.attempt_token=$1 AND a.submitted_at IS NULL FOR UPDATE OF a`, t).Scan(&id, &revision, &expires)
	if err != nil {
		write(w, 409, map[string]string{"error": "Attempt expired or unavailable", "code": "attempt_closed"})
		return
	}
	if time.Now().After(expires) {
		write(w, 409, map[string]string{"error": "Time is up — answers can no longer be saved", "code": "expired"})
		return
	}
	var kind string
	var opts json.RawMessage
	if err = tx.QueryRow(r.Context(), `SELECT kind,options FROM attempt_questions WHERE attempt_id=$1 AND question_id=$2`, id, p.QuestionID).Scan(&kind, &opts); err != nil {
		write(w, 409, map[string]string{"error": "Question unavailable", "code": "question_missing"})
		return
	}
	if verr := validateAnswer(kind, p.Value, opts, ""); verr != nil {
		write(w, 400, map[string]string{"error": "Invalid answer: " + verr.Error(), "code": "invalid_answer"})
		return
	}
	// Optimistic concurrency: a client working from a stale snapshot (another
	// tab/device saved meanwhile) must reload instead of silently overwriting.
	if p.BaseRevision != nil && *p.BaseRevision != revision {
		write(w, 409, map[string]any{"error": "Answers changed elsewhere — reload before saving", "code": "stale_revision", "revision": revision})
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO answers(attempt_id,question_id,value) VALUES($1,$2,$3) ON CONFLICT(attempt_id,question_id) DO UPDATE SET value=excluded.value`, id, p.QuestionID, p.Value); err != nil {
		fail(w, 500, "Save failed")
		return
	}
	var newRevision int64
	if err = tx.QueryRow(r.Context(), `UPDATE attempts SET answer_revision=answer_revision+1 WHERE id=$1 RETURNING answer_revision`, id).Scan(&newRevision); err != nil {
		fail(w, 500, "Save failed")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Save failed")
		return
	}
	write(w, 200, map[string]any{"saved": true, "revision": newRevision})
}
func (s *server) results(w http.ResponseWriter, r *http.Request, id string) {
	rows, e := s.db.Query(r.Context(), `SELECT a.id,a.student_name,e.title,a.started_at,a.submitted_at,a.score,a.total FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE e.teacher_id=$1 ORDER BY a.started_at DESC LIMIT 200`, id)
	if e != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var aid, name, title string
		var started time.Time
		var submitted *time.Time
		var score, total *int
		_ = rows.Scan(&aid, &name, &title, &started, &submitted, &score, &total)
		out = append(out, map[string]any{"id": aid, "studentName": name, "title": title, "startedAt": started, "submittedAt": submitted, "score": score, "total": total})
	}
	write(w, 200, out)
}

// review returns answers and grading information only to the exam's owner.
func (s *server) review(w http.ResponseWriter, r *http.Request, teacherID string) {
	aid := chi.URLParam(r, "id")
	var name, title string
	var submitted *time.Time
	var score, total *int
	err := s.db.QueryRow(r.Context(), `SELECT a.student_name,e.title,a.submitted_at,a.score,a.total FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.id=$1 AND e.teacher_id=$2`, aid, teacherID).Scan(&name, &title, &submitted, &score, &total)
	if err != nil {
		fail(w, 404, "Attempt not found")
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT aq.question_id,aq.kind,aq.prompt,aq.correct_answer,aq.points,aq.options,coalesce(ans.value,''),g.points,g.feedback FROM attempt_questions aq LEFT JOIN answers ans ON ans.question_id=aq.question_id AND ans.attempt_id=aq.attempt_id LEFT JOIN grading_records g ON g.question_id=aq.question_id AND g.attempt_id=aq.attempt_id WHERE aq.attempt_id=$1 ORDER BY aq.position,aq.question_id`, aid)
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer rows.Close()
	qs := []any{}
	for rows.Next() {
		var qid, kind, prompt, correct, value string
		var points int
		var opts json.RawMessage
		var grade *int
		var feedback *string
		if err := rows.Scan(&qid, &kind, &prompt, &correct, &points, &opts, &value, &grade, &feedback); err != nil {
			fail(w, 500, "Database error")
			return
		}
		qs = append(qs, map[string]any{"id": qid, "kind": kind, "prompt": prompt, "correctAnswer": correct, "options": opts, "points": points, "value": value, "manualPoints": grade, "feedback": feedback})
	}
	write(w, 200, map[string]any{"id": aid, "studentName": name, "title": title, "submittedAt": submitted, "score": score, "total": total, "questions": qs})
}
func (s *server) grade(w http.ResponseWriter, r *http.Request, teacherID string) {
	aid := chi.URLParam(r, "id")
	qid := chi.URLParam(r, "questionID")
	var p struct {
		Points   int
		Feedback string
	}
	if decode(r, &p) != nil || p.Points < 0 || len(p.Feedback) > 2000 {
		fail(w, 400, "Invalid grade")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, 500, "Database error")
		return
	}
	defer tx.Rollback(r.Context())
	var attemptID string
	err = tx.QueryRow(r.Context(), `SELECT a.id FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.id=$1 AND e.teacher_id=$2 AND a.submitted_at IS NOT NULL FOR UPDATE OF a`, aid, teacherID).Scan(&attemptID)
	if err != nil {
		fail(w, 404, "Submitted attempt not found")
		return
	}
	var max int
	err = tx.QueryRow(r.Context(), `SELECT points FROM attempt_questions WHERE attempt_id=$1 AND question_id=$2 AND kind='essay'`, aid, qid).Scan(&max)
	if err != nil {
		fail(w, 404, "Essay question not found")
		return
	}
	if p.Points > max {
		fail(w, 400, "Points exceed question maximum")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO grading_records(attempt_id,question_id,teacher_id,points,feedback) VALUES($1,$2,$3,$4,$5) ON CONFLICT(attempt_id,question_id) DO UPDATE SET points=excluded.points,feedback=excluded.feedback,graded_at=now()`, aid, qid, teacherID, p.Points, p.Feedback)
	if err != nil {
		fail(w, 500, "Grade failed")
		return
	}
	var score, total int
	if err = tx.QueryRow(r.Context(), gradeSQL, aid).Scan(&score, &total); err != nil {
		fail(w, 500, "Grade failed")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE attempts SET score=$2,total=$3 WHERE id=$1`, aid, score, total)
	if err != nil {
		fail(w, 500, "Grade failed")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, 500, "Grade failed")
		return
	}
	s.audit(teacherID, "result.graded", "attempt", aid)
	write(w, 200, map[string]bool{"ok": true})
}
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL required")
		os.Exit(1)
	}
	db, e := pgxpool.New(context.Background(), dbURL)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	s := &server{db}
	r := chi.NewRouter()
	r.Use(security.OriginGuard)
	r.Use(security.CSRFProtect)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
			if os.Getenv("COOKIE_SECURE") == "true" {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			rows, e := db.Query(ctx, `SELECT a.attempt_token FROM attempts a JOIN exams e ON e.id=a.exam_id WHERE a.submitted_at IS NULL AND (a.expires_at <= now() OR (e.ends_at IS NOT NULL AND e.ends_at <= now())) LIMIT 100`)
			tokens := []string{}
			if e == nil {
				for rows.Next() {
					var t string
					if rows.Scan(&t) == nil {
						tokens = append(tokens, t)
					}
				}
				rows.Close()
			}
			for _, t := range tokens {
				if e := s.finalize(ctx, t); e != nil {
					slog.Error("deadline processing failed", "error", e)
				}
			}
			cancel()
		}
	}()
	r.Get("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if db.Ping(r.Context()) != nil {
			fail(w, 503, "Database unavailable")
			return
		}
		write(w, 200, map[string]bool{"ok": true})
	})
	r.With(security.NewLimiter(6, time.Minute).Wrap).Post("/api/v1/auth/register", s.auth)
	r.With(security.NewLimiter(10, time.Minute).Wrap).Post("/api/v1/auth/login", s.auth)
	r.With(security.NewLimiter(5, time.Minute).Wrap).Post("/api/v1/auth/forgot-password", s.forgotPassword)
	r.With(security.NewLimiter(10, time.Minute).Wrap).Post("/api/v1/auth/reset-password", s.resetPassword)
	r.Post("/api/v1/uploads/images", s.protected(s.uploadImage))
	r.Get("/uploads/{file}", s.serveUpload)
	r.Get("/api/v1/auth/me", s.protected(s.me))
	r.Get("/api/v1/account/profile", s.protected(s.profile))
	r.Put("/api/v1/account/profile", s.protected(s.profile))
	r.Post("/api/v1/account/password", s.protected(s.changePassword))
	r.Get("/api/v1/public/teachers/{slug}", s.publicTeacher)
	r.Get("/api/v1/admin/overview", s.admin(s.adminOverview))
	r.Get("/api/v1/admin/users", s.admin(s.adminUsers))
	r.Put("/api/v1/admin/users/{id}/status", s.admin(s.adminAccountStatus))
	r.Get("/api/v1/admin/audit", s.admin(s.adminAudit))
	r.Get("/api/v1/admin/platform", s.admin(s.adminPlatform))
	r.Put("/api/v1/admin/platform", s.admin(s.adminPlatform))
	r.Put("/api/v1/admin/users/{id}/role", s.admin(s.adminUserRole))
	r.Put("/api/v1/admin/users/{id}/plan", s.admin(s.adminUserPlan))
	r.Post("/api/v1/auth/logout", s.protected(s.logout))
	r.Get("/api/v1/exams", s.protected(s.exams))
	r.Post("/api/v1/exams", s.protected(s.exams))
	r.Get("/api/v1/exams/{id}", s.protected(s.exam))
	r.Put("/api/v1/exams/{id}", s.protected(s.updateExam))
	r.Delete("/api/v1/exams/{id}", s.protected(s.deleteExam))
	r.Post("/api/v1/exams/{id}/duplicate", s.protected(s.duplicateExam))
	r.Post("/api/v1/exams/{id}/status/{action}", s.protected(s.changeStatus))
	r.Post("/api/v1/exams/{id}/publish", s.protected(s.exam))
	r.Post("/api/v1/exams/{id}/questions", s.protected(s.question))
	r.Put("/api/v1/exams/{id}/questions/order", s.protected(s.reorderQuestions))
	r.Put("/api/v1/exams/{id}/questions/{questionID}", s.protected(s.editQuestion))
	r.Delete("/api/v1/exams/{id}/questions/{questionID}", s.protected(s.editQuestion))
	r.Post("/api/v1/exams/{id}/questions/{questionID}/duplicate", s.protected(s.duplicateQuestion))
	r.Get("/api/v1/students", s.protected(s.students))
	r.Post("/api/v1/students", s.protected(s.students))
	r.Get("/api/v1/students/export", s.protected(s.exportStudents))
	r.Post("/api/v1/students/import", s.protected(s.importStudents))
	r.Get("/api/v1/students/{id}", s.protected(s.student))
	r.Put("/api/v1/students/{id}", s.protected(s.student))
	r.Delete("/api/v1/students/{id}", s.protected(s.student))
	r.Get("/api/v1/students/{id}/attempts", s.protected(s.studentAttempts))
	r.Get("/api/v1/classes", s.protected(s.classes))
	r.Post("/api/v1/classes", s.protected(s.classes))
	r.Get("/api/v1/classes/{id}", s.protected(s.class))
	r.Put("/api/v1/classes/{id}", s.protected(s.class))
	r.Delete("/api/v1/classes/{id}", s.protected(s.class))
	r.Post("/api/v1/classes/{id}/members", s.protected(s.classMembers))
	r.Delete("/api/v1/classes/{id}/members/{studentID}", s.protected(s.classMembers))
	r.Get("/api/v1/classes/{id}/report", s.protected(s.classReport))
	r.Put("/api/v1/exams/{id}/classes", s.protected(s.setExamClasses))
	r.Get("/api/v1/exams/{id}/classes", s.protected(s.examClasses))
	r.Put("/api/v1/exams/{id}/result-visibility", s.protected(s.setResultVisibility))
	r.Get("/api/v1/analytics", s.protected(s.analytics))
	r.Get("/api/v1/results", s.protected(s.results))
	r.Get("/api/v1/results/{id}", s.protected(s.review))
	r.Put("/api/v1/results/{id}/questions/{questionID}/grade", s.protected(s.grade))
	r.Get("/api/v1/public/exams/{token}", s.public)
	r.With(security.NewLimiter(20, time.Minute).Wrap).Post("/api/v1/public/exams/{token}/start", s.start)
	r.Get("/api/v1/attempts/{token}", s.attempt)
	r.Put("/api/v1/attempts/{token}/answers", s.answer)
	r.Post("/api/v1/attempts/{token}/submit", func(w http.ResponseWriter, r *http.Request) {
		e := s.finalize(r.Context(), chi.URLParam(r, "token"))
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				fail(w, 404, "Attempt not found")
			} else {
				fail(w, 500, "Submission failed")
			}
			return
		}
		write(w, 200, map[string]bool{"submitted": true})
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if _, e := strconv.Atoi(port); e != nil {
		panic(e)
	}
	slog.Info("Listening", "port", port)
	panic(http.ListenAndServe(":"+port, r))
}
