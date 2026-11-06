-- Exam/attempt integrity: question snapshots, answer revisions, new question kinds,
-- password recovery tokens and plan entitlement columns.
ALTER TABLE attempts ADD COLUMN answer_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE questions ADD COLUMN image_url text NOT NULL DEFAULT '';
ALTER TABLE questions DROP CONSTRAINT questions_kind_check;
ALTER TABLE questions ADD CONSTRAINT questions_kind_check CHECK (kind IN ('single','multi','truefalse','short','fill','numeric','essay','matching','ordering','image'));
-- Immutable per-attempt snapshot of the question set. Active and completed attempts
-- read and grade exclusively from this table, so later exam edits never change them.
CREATE TABLE attempt_questions (
  attempt_id uuid NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
  question_id uuid NOT NULL REFERENCES questions(id),
  kind text NOT NULL,
  prompt text NOT NULL,
  options jsonb NOT NULL DEFAULT '[]',
  correct_answer text NOT NULL DEFAULT '',
  image_url text NOT NULL DEFAULT '',
  points int NOT NULL CHECK (points > 0),
  position int NOT NULL DEFAULT 0,
  PRIMARY KEY (attempt_id, question_id)
);
CREATE INDEX attempt_questions_order ON attempt_questions(attempt_id, position, question_id);
CREATE TABLE password_reset_tokens (
  token_hash text PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX password_reset_tokens_user ON password_reset_tokens(user_id, created_at DESC);
ALTER TABLE users ADD COLUMN plan text NOT NULL DEFAULT 'free' CHECK (plan IN ('free','pro'));
ALTER TABLE users ADD COLUMN plan_expires_at timestamptz;
CREATE INDEX users_active_plan ON users(plan) WHERE suspended_at IS NULL;
-- Backfill snapshots for attempts that predate this migration so their
-- historical state and grading remain available (best-effort snapshot of the
-- current question set at migration time).
INSERT INTO attempt_questions(attempt_id,question_id,kind,prompt,options,correct_answer,image_url,points,position)
SELECT a.id,q.id,q.kind,q.prompt,q.options,q.correct_answer,q.image_url,q.points,q.position
FROM attempts a JOIN questions q ON q.exam_id=a.exam_id
ON CONFLICT DO NOTHING;
