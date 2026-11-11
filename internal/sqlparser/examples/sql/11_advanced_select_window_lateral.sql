WITH RECURSIVE active_users AS (
  SELECT id, email, profile
  FROM users
  WHERE profile @> '{"active": true}'
),
ranked_orders AS (
  SELECT
    user_id,
    total,
    created_at,
    COUNT(*) FILTER (WHERE total > 0) OVER user_order_window AS paid_order_count,
    ROW_NUMBER() OVER (
      PARTITION BY user_id
      ORDER BY created_at DESC
    ) AS recency_rank
  FROM orders
  WINDOW user_order_window AS (
    PARTITION BY user_id
    ORDER BY created_at DESC
  )
)
SELECT DISTINCT ON (u.id)
  u.*,
  r.paid_order_count,
  recent.total AS last_total,
  v.label
FROM active_users u (id, email, profile)
LEFT JOIN LATERAL (
  SELECT total
  FROM ranked_orders ro
  WHERE ro.user_id = u.id
  ORDER BY ro.created_at DESC NULLS LAST
  LIMIT 1
) recent (total) ON true
JOIN ranked_orders r ON r.user_id = u.id
JOIN (VALUES (1, 'gold'), (2, 'silver'), (3, 'bronze')) v (id, label) ON v.id = u.id
WHERE EXISTS (
  SELECT 1
  FROM orders o
  WHERE o.user_id = u.id
)
ORDER BY u.id ASC NULLS LAST
LIMIT 50
FOR UPDATE OF u SKIP LOCKED;
