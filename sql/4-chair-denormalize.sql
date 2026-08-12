SET CHARACTER_SET_CLIENT = utf8mb4;
SET CHARACTER_SET_CONNECTION = utf8mb4;

USE isuride;

-- chair_locations の履歴を毎回集計せずに済ませるための非正規化。
-- 3-initial-data.sql の INSERT はカラム名を省略しているため、1-schema.sql で列を足すと
-- 値の数が合わずに投入が失敗する。そのため列の追加はデータ投入後のここで行う。
ALTER TABLE chairs
  ADD COLUMN latitude            INTEGER     NULL COMMENT '最新の緯度',
  ADD COLUMN longitude           INTEGER     NULL COMMENT '最新の経度',
  ADD COLUMN total_distance      INTEGER     NOT NULL DEFAULT 0 COMMENT '総移動距離',
  ADD COLUMN location_updated_at DATETIME(6) NULL COMMENT '座標の最終更新日時';

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
