REPLACE INTO user_archive (id, name)
VALUES (101, 'archived-user');

SHOW TABLES LIKE 'user%';
EXPLAIN SELECT * FROM user_archive WHERE id BETWEEN 10 AND 20;
