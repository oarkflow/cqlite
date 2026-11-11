CREATE TABLE IF NOT EXISTS events (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  tenant_id BIGINT NOT NULL,
  payload JSONB,
  created_at DATETIME
) ENGINE=InnoDB;

CREATE UNIQUE INDEX idx_events_tenant_id ON events (tenant_id, id DESC);

CREATE OR REPLACE VIEW active_events AS
SELECT id, payload FROM events WHERE payload @> '{"active": true}';
