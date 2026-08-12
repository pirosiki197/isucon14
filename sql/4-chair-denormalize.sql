SET CHARACTER_SET_CLIENT = utf8mb4;
SET CHARACTER_SET_CONNECTION = utf8mb4;

USE isuride;

-- アプリは座標を受け取るたびに chairs 側を更新するが、初期データは chair_locations に
-- しか入っていない。ここで履歴を畳んで chairs に移しておく。

UPDATE chairs c
  JOIN (SELECT chair_id,
               SUM(IFNULL(distance, 0)) AS total_distance,
               MAX(created_at)          AS last_at
        FROM (SELECT chair_id,
                     created_at,
                     ABS(latitude - LAG(latitude) OVER (PARTITION BY chair_id ORDER BY created_at)) +
                     ABS(longitude - LAG(longitude) OVER (PARTITION BY chair_id ORDER BY created_at)) AS distance
              FROM chair_locations) t
        GROUP BY chair_id) d ON d.chair_id = c.id
SET c.total_distance      = d.total_distance,
    c.location_updated_at = d.last_at;

UPDATE chairs c
  JOIN chair_locations l ON l.chair_id = c.id
  JOIN (SELECT chair_id, MAX(created_at) AS mx FROM chair_locations GROUP BY chair_id) m
    ON m.chair_id = l.chair_id AND m.mx = l.created_at
SET c.latitude  = l.latitude,
    c.longitude = l.longitude;
