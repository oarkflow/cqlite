INSERT INTO users (id, name)
VALUES (1, COALESCE($1, 'unknown'))
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name;
