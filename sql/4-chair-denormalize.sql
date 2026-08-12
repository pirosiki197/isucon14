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
  ADD COLUMN location_updated_at DATETIME(6) NULL COMMENT '座標の最終更新日時',
  ADD COLUMN pending_rides       INTEGER     NOT NULL DEFAULT 0 COMMENT '椅子への通知が終わっていないライド数',
  ADD COLUMN completed_rides     INTEGER     NOT NULL DEFAULT 0 COMMENT '完了をユーザーに通知済みのライド数',
  ADD COLUMN total_evaluation    INTEGER     NOT NULL DEFAULT 0 COMMENT '上記ライドの評価の合計',
  ADD INDEX idx_is_active_pending_rides (is_active, pending_rides);

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

-- ride_statuses は追記型で、現在の状態を毎回 ORDER BY created_at DESC LIMIT 1 で求めていた。
-- rides 側に最新の状態を持たせる。3-initial-data.sql の rides の INSERT もカラム名を
-- 省略しているため、ここで追加する。
ALTER TABLE rides
  ADD COLUMN latest_status ENUM ('MATCHING', 'ENROUTE', 'PICKUP', 'CARRYING', 'ARRIVED', 'COMPLETED')
    NOT NULL DEFAULT 'MATCHING' COMMENT '最新の状態',
  ADD INDEX idx_chair_id_latest_status (chair_id, latest_status);

-- updated_at は ON UPDATE CURRENT_TIMESTAMP なので、明示的に元の値を入れて自動更新を抑える。
-- ownerGetSales が rides.updated_at で期間を絞るため、ここで書き換わると売上が壊れる。
UPDATE rides r
  JOIN (SELECT s.ride_id, s.status
        FROM ride_statuses s
          JOIN (SELECT ride_id, MAX(created_at) AS mx FROM ride_statuses GROUP BY ride_id) t
            ON t.ride_id = s.ride_id AND t.mx = s.created_at) l ON l.ride_id = r.id
SET r.latest_status = l.status,
    r.updated_at    = r.updated_at;

-- 椅子の統計は通知のたびに集計していた。評価を受け付けた時点で加算する形に移す。
-- 完走の判定は元の集計と同じで、ARRIVED / CARRYING / COMPLETED の 3 つが揃うこと。
-- 対象ライドに evaluation が NULL のものは無いため、合計 ÷ 件数は AVG と一致する。
UPDATE chairs c
  JOIN (SELECT chair_id, COUNT(*) AS cnt, SUM(evaluation) AS total
        FROM (SELECT r.chair_id, r.id, r.evaluation
              FROM rides r
                     JOIN ride_statuses s ON s.ride_id = r.id
              WHERE r.chair_id IS NOT NULL
              GROUP BY r.chair_id, r.id, r.evaluation
              HAVING SUM(s.status = 'ARRIVED') > 0
                 AND SUM(s.status = 'CARRYING') > 0
                 AND SUM(s.status = 'COMPLETED') > 0) q
        GROUP BY chair_id) t ON t.chair_id = c.id
SET c.completed_rides  = t.cnt,
    c.total_evaluation = t.total;

-- マッチングは「担当ライド全ての 6 状態を椅子へ通知し終えた椅子」だけを空きとみなす。
-- その判定を毎回集計せずに済ませるため、未通知のライド数を数えて持たせる。
UPDATE chairs c
  JOIN (SELECT r.chair_id, COUNT(*) AS cnt
        FROM rides r
        WHERE r.chair_id IS NOT NULL
          AND (SELECT COUNT(s.chair_sent_at) FROM ride_statuses s WHERE s.ride_id = r.id) <> 6
        GROUP BY r.chair_id) p ON p.chair_id = c.id
SET c.pending_rides = p.cnt;
