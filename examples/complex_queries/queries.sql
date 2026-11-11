-- SQLite-compatible complex query examples for github.com/oarkflow/sqlite.

CREATE TABLE IF NOT EXISTS accounts (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orders (
  id INTEGER PRIMARY KEY,
  account_id INTEGER NOT NULL,
  status TEXT NOT NULL,
  total REAL NOT NULL,
  created_at TEXT NOT NULL,
  metadata TEXT
);

CREATE TABLE IF NOT EXISTS account_settings (
  account_id INTEGER PRIMARY KEY,
  retention_days INTEGER NOT NULL,
  updated_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS system_settings (
  tenant_id INTEGER PRIMARY KEY,
  days INTEGER NOT NULL,
  hours INTEGER NOT NULL
);

CREATE INDEX idx_orders_account_status_param
ON orders (account_id, status);

DROP VIEW IF EXISTS active_orders;

CREATE VIEW active_orders AS
SELECT id, account_id, status, total
FROM orders
WHERE status = 'open';

-- Mixed named and positional binds. Use sqlite.Named("since", value), not $1,
-- unless you intentionally pass sqlite.Named("1", value).
SELECT id, account_id, status, total
FROM orders
WHERE account_id = :account_id
  AND status = @status
  AND created_at >= $since
  AND total >= ?
ORDER BY id;

-- Insert with named and positional values. Use a follow-up SELECT instead of RETURNING.
INSERT INTO orders (id, account_id, status, total, created_at, metadata)
VALUES (?, :account_id, @status, ?, :created_at, ?);

SELECT id, account_id, status, total
FROM orders
WHERE id = ?;

-- Struct or map backed single-row insert. The object must provide the listed columns.
INSERT INTO orders (id, account_id, status, total, created_at, metadata)
VALUES :row;

-- Slice/array backed multi-row insert. Each element can be a map or struct.
INSERT INTO orders (id, account_id, status, total, created_at, metadata)
VALUES :rows;

-- Portable settings upsert expressed as update-then-insert.
UPDATE account_settings
SET retention_days = :retention_days,
    updated_by = @actor
WHERE account_id = :account_id;

INSERT INTO account_settings (account_id, retention_days, updated_by)
VALUES (:account_id, :retention_days, @actor);

-- Mixed bind update.
UPDATE orders
SET status = :next_status,
    total = total + :adjustment,
    metadata = ?
WHERE account_id = :account_id
  AND id = $id;

SELECT id, status, total
FROM orders
WHERE id = $id;

-- Delete with pre-selected returned ids.
SELECT id
FROM orders
WHERE account_id = :account_id
  AND status = @deleted_status
  AND created_at < ?;

DELETE FROM orders
WHERE account_id = :account_id
  AND status = @deleted_status
  AND created_at < ?;

-- Join, aggregate, subquery, grouping, ordering, and HAVING.
SELECT accounts.name, count(*), sum(orders.total)
FROM orders
JOIN accounts ON orders.account_id = accounts.id
WHERE orders.account_id = :account_id
  AND orders.id IN (
    SELECT id
    FROM orders
    WHERE total >= ?
  )
GROUP BY accounts.name
HAVING sum(orders.total) >= ?
ORDER BY accounts.name;

-- EXISTS predicate.
SELECT name
FROM accounts
WHERE EXISTS (
  SELECT id
  FROM orders
  WHERE orders.account_id = accounts.id
    AND orders.status = @status
)
ORDER BY name;

-- Query planner diagnostic.
EXPLAIN QUERY PLAN
SELECT id
FROM orders
WHERE account_id = :account_id
  AND status = @status;
