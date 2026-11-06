-- Platform configuration and plan entitlements.
CREATE TABLE platform_config (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  registration_open boolean NOT NULL DEFAULT true,
  maintenance_mode boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO platform_config (id) VALUES (1);
-- Free-plan entitlements (enforced server-side; overridable per admin grant).
ALTER TABLE users ADD COLUMN plan_updated_at timestamptz;
