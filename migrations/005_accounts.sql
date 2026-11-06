ALTER TABLE users ADD COLUMN bio text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN academy_name text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN public_slug text UNIQUE;
ALTER TABLE users ADD COLUMN suspended_at timestamptz;
CREATE TABLE audit_logs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 actor_id uuid REFERENCES users(id) ON DELETE SET NULL,
 action text NOT NULL,
 entity_type text NOT NULL,
 entity_id uuid,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_actor_created ON audit_logs(actor_id,created_at DESC);
