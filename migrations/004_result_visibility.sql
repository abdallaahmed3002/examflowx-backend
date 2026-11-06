ALTER TABLE exams ADD COLUMN result_visibility text NOT NULL DEFAULT 'immediate' CHECK(result_visibility IN ('immediate','teacher_only'));
