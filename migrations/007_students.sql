-- Roster, classes/groups and group-based exam assignment.
CREATE TABLE students (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  teacher_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  email text NOT NULL DEFAULT '',
  external_id text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX students_teacher_name ON students(teacher_id, lower(name));
CREATE INDEX students_teacher_created ON students(teacher_id, created_at DESC);
CREATE TABLE classes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  teacher_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX classes_teacher_name ON classes(teacher_id, lower(name));
CREATE TABLE class_members (
  class_id uuid NOT NULL REFERENCES classes(id) ON DELETE CASCADE,
  student_id uuid NOT NULL REFERENCES students(id) ON DELETE CASCADE,
  PRIMARY KEY (class_id, student_id)
);
CREATE INDEX class_members_student ON class_members(student_id);
CREATE TABLE exam_classes (
  exam_id uuid NOT NULL REFERENCES exams(id) ON DELETE CASCADE,
  class_id uuid NOT NULL REFERENCES classes(id) ON DELETE CASCADE,
  PRIMARY KEY (exam_id, class_id)
);
CREATE INDEX exam_classes_class ON exam_classes(class_id);
ALTER TABLE attempts ADD COLUMN student_id uuid REFERENCES students(id) ON DELETE SET NULL;
CREATE INDEX attempts_student ON attempts(student_id, started_at DESC);
