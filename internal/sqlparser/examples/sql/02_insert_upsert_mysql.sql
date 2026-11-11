INSERT INTO users (id, name, updated_at)
VALUES (1, IFNULL(:name, 'unknown'), NOW())
ON DUPLICATE KEY UPDATE
  name = IFNULL(:name, name),
  updated_at = NOW();
