CREATE TABLE grading_records (
 attempt_id uuid NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
 question_id uuid NOT NULL REFERENCES questions(id),
 teacher_id uuid NOT NULL REFERENCES users(id),
 points int NOT NULL CHECK(points >= 0),
 feedback text NOT NULL DEFAULT '',
 graded_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(attempt_id,question_id)
);
CREATE INDEX grading_records_teacher ON grading_records(teacher_id,graded_at DESC);
