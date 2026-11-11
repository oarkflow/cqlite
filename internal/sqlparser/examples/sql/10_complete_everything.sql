CREATE DATABASE IF NOT EXISTS appdb;
USE appdb;

CREATE TABLE users (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  email VARCHAR(255) NOT NULL,
  profile JSONB,
  created_at DATETIME
) ENGINE=InnoDB;

CREATE TABLE orders (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  total DECIMAL(12,2) NOT NULL,
  status VARCHAR(32) NOT NULL,
  payload JSONB,
  created_at DATETIME,
  CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB;

CREATE UNIQUE INDEX idx_users_email ON users (email);
CREATE INDEX idx_orders_user_created ON orders (user_id, created_at DESC);

WITH active_users AS (
  SELECT id, email, profile FROM users WHERE profile @> '{"active": true}'
),
recent_orders AS (
  SELECT user_id, COUNT(*) AS cnt, SUM(total) AS sum_total
  FROM orders
  GROUP BY user_id
)
SELECT u.id,
       u.email,
       IFNULL(r.cnt, 0) AS order_count,
       COALESCE(r.sum_total, 0) AS total_spent,
       u.profile->>'tier' AS tier
FROM active_users u
LEFT JOIN recent_orders r ON u.id = r.user_id
WHERE u.email LIKE '%@example.com'
UNION ALL
SELECT id, email, 0, 0, profile->>'tier' FROM users WHERE id IN (SELECT id FROM active_users)
INTERSECT
SELECT id, email, 0, 0, profile->>'tier' FROM users;

INSERT INTO users (id, email, profile, created_at)
VALUES (1, 'a@example.com', '{"active": true, "tier": "gold"}', NOW())
ON DUPLICATE KEY UPDATE
  profile = profile || '{"active": true}',
  created_at = NOW();

INSERT INTO users (id, email, profile)
VALUES (2, 'b@example.com', '{"active": false, "tier": "silver"}')
ON CONFLICT (id) DO UPDATE SET
  profile = EXCLUDED.profile;

UPDATE users
SET profile = profile || '{"last_login": "2026-01-01"}'
WHERE profile ? 'active' AND id BETWEEN 1 AND 100
ORDER BY id ASC
LIMIT 10;

DELETE FROM orders
WHERE payload #>> '{meta,status}' = 'cancelled'
ORDER BY id ASC
LIMIT 100;

REPLACE INTO users (id, email, profile)
VALUES (3, 'replace@example.com', '{"active": true}');

CREATE OR REPLACE VIEW user_order_summary AS
SELECT u.id, u.email, COUNT(o.id) AS order_count
FROM users u
LEFT JOIN orders o ON u.id = o.user_id
GROUP BY u.id, u.email;

ALTER TABLE users ADD COLUMN flags JSONB;
ALTER TABLE users MODIFY COLUMN email VARCHAR(320);
ALTER TABLE users ADD INDEX idx_users_created (created_at);
ALTER TABLE users RENAME TO app_users;

SHOW TABLES LIKE 'user%';
EXPLAIN SELECT * FROM app_users WHERE id = 1;

START TRANSACTION READ WRITE;
SAVEPOINT sp1;
CALL refresh_user_summary('daily');
ROLLBACK TO SAVEPOINT sp1;
COMMIT;

CREATE FUNCTION normalize_email;
DROP TRIGGER trg_before_insert_users;

DROP INDEX idx_orders_user_created ON orders;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS app_users;
DROP DATABASE IF EXISTS appdb;
