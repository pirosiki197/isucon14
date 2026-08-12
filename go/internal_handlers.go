package main

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	// 待ち時間 1 秒あたりコストから差し引く量。古いライドを優先させて飢餓を防ぐ。
	matchingAgingWeight = 2.0
	// 座標を一度も報告していない椅子のコスト。他に候補が無いときだけ選ばれるようにする。
	matchingUnknownLocationCost = 1000.0
)

// 呼び出しが重なっても同じ椅子を二重に割り当てないようにする
var matchingMu sync.Mutex

type matchingChair struct {
	ID        string `db:"id"`
	Latitude  *int   `db:"latitude"`
	Longitude *int   `db:"longitude"`
	Speed     int    `db:"speed"`
}

type matchingPair struct {
	rideIndex  int
	chairIndex int
	cost       float64
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	matchingMu.Lock()
	defer matchingMu.Unlock()

	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	chairs := []matchingChair{}
	if err := db.SelectContext(ctx, &chairs, `
SELECT c.id, c.latitude, c.longitude, m.speed
FROM chairs c
  JOIN chair_models m ON m.name = c.model
WHERE c.is_active = TRUE AND c.pending_rides = 0`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	for _, pair := range assignRides(rides, chairs, time.Now()) {
		chairID := chairs[pair.chairIndex].ID
		if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", chairID, rides[pair.rideIndex].ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := tx.ExecContext(ctx, "UPDATE chairs SET pending_rides = pending_rides + 1 WHERE id = ?", chairID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// 待機ライドと空き椅子の全組み合わせをコストの小さい順に見て貪欲に確定させる。
// コストは迎車と乗車に掛かる所要時間で、椅子の速度が違うため距離ではなく時間で比べる。
func assignRides(rides []Ride, chairs []matchingChair, now time.Time) []matchingPair {
	pairs := make([]matchingPair, 0, len(rides)*len(chairs))
	for i := range rides {
		rideDistance := calculateDistance(
			rides[i].PickupLatitude, rides[i].PickupLongitude,
			rides[i].DestinationLatitude, rides[i].DestinationLongitude,
		)
		aging := matchingAgingWeight * now.Sub(rides[i].CreatedAt).Seconds()
		for j := range chairs {
			cost := matchingUnknownLocationCost
			if chairs[j].Latitude != nil && chairs[j].Longitude != nil {
				pickupDistance := calculateDistance(
					*chairs[j].Latitude, *chairs[j].Longitude,
					rides[i].PickupLatitude, rides[i].PickupLongitude,
				)
				cost = float64(pickupDistance+rideDistance) / float64(chairs[j].Speed)
			}
			pairs = append(pairs, matchingPair{rideIndex: i, chairIndex: j, cost: cost - aging})
		}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].cost < pairs[b].cost })

	maxAssign := min(len(rides), len(chairs))
	assigned := make([]matchingPair, 0, maxAssign)
	rideTaken := make([]bool, len(rides))
	chairTaken := make([]bool, len(chairs))
	for _, pair := range pairs {
		if rideTaken[pair.rideIndex] || chairTaken[pair.chairIndex] {
			continue
		}
		rideTaken[pair.rideIndex] = true
		chairTaken[pair.chairIndex] = true
		assigned = append(assigned, pair)
		if len(assigned) == maxAssign {
			break
		}
	}
	return assigned
}
