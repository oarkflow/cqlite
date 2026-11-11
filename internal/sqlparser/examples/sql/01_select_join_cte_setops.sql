WITH active_users AS (
  SELECT id, name FROM users WHERE active = 1
),
recent_orders AS (
  SELECT user_id, COUNT(*) AS cnt FROM orders GROUP BY user_id
)
SELECT u.id, u.name, r.cnt
FROM active_users u
LEFT JOIN recent_orders r ON u.id = r.user_id
WHERE r.cnt > 0
UNION ALL
SELECT id, name, 0 FROM archived_users
INTERSECT
SELECT id, name, 0 FROM users;
