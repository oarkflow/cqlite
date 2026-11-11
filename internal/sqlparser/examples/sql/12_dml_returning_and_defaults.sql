INSERT INTO audit_log DEFAULT VALUES
RETURNING id, created_at;

INSERT INTO users SET
  id = 101,
  email = 'new@example.com',
  profile = '{"active": true}'
ON CONFLICT (id) WHERE id > 0 DO UPDATE SET
  email = EXCLUDED.email,
  profile = EXCLUDED.profile
WHERE users.email <> EXCLUDED.email
RETURNING id, email, profile;

UPDATE users u
SET u.email = LOWER(u.email),
    u.profile = u.profile || '{"normalized": true}'
WHERE u.profile ? 'active'
RETURNING id, email, profile;

DELETE u
FROM users u
JOIN orders o ON o.user_id = u.id
WHERE o.status = 'cancelled'
RETURNING id, email;
