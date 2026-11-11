UPDATE events
SET payload = payload || '{"processed": true}'
WHERE payload ? 'user' AND payload @> '{"active": true}';

DELETE FROM events
WHERE payload #>> '{meta,status}' = 'deleted';
