package nowplaying

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

/*
 * 表示用のデータを組み立てる。
 *
 * # 取り直しを裏に回す理由
 *
 * 原神系のプラグインは「ジョブで定期取得 → 表示は DB から」だが、再生中の曲は
 * 数分で変わるので先回りしても当たらない。かといってプロフィールを開くたびに
 * 外へ出ると、取得元が遅い日にプロフィール全体が待たされる。
 *
 * そこで **手元のものを即返し、期限切れなら裏で取り直す**。表示は 1 回ぶん
 * 古いことがあるが、待たされない。
 */

// refreshGuard prevents piling up background fetches for the same user.
//
// プロフィールが連打されたときに、同じ利用者の取得が何本も走らないようにする。
type refreshGuard struct {
	mu    sync.Mutex
	inFly map[string]struct{}
}

func newRefreshGuard() *refreshGuard {
	return &refreshGuard{inFly: map[string]struct{}{}}
}

// enter reports whether the caller may start a fetch for key.
func (g *refreshGuard) enter(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, busy := g.inFly[key]; busy {
		return false
	}
	g.inFly[key] = struct{}{}
	return true
}

func (g *refreshGuard) leave(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inFly, key)
}

// buildProfile assembles the display payload for a local user.
//
// 未登録なら (nil, nil)。**エラーと区別する** — 「登録していない」は普通の
// 状態で、表示側はそれを見て何も描かない。
func buildProfile(c context.Context, db *sql.DB, cl *client, userID string) (map[string]any, error) {
	var service, username string
	err := db.QueryRowContext(c,
		`SELECT service, username FROM accounts WHERE user_id = $1`, userID).Scan(&service, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var playingRaw, recentRaw []byte
	var fetchedAt time.Time
	var expired bool
	err = db.QueryRowContext(c, `
		SELECT playing, recent, fetched_at, expires_at <= now()
		FROM snapshots WHERE user_id = $1
	`, userID).Scan(&playingRaw, &recentRaw, &fetchedAt, &expired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 手元に何も無いときだけ**その場で取る**。ここで裏に回すと、登録した
		// 直後に何も出ない状態が続いて壊れて見える。
		snap, ferr := cl.fetch(c, service, username)
		if ferr != nil {
			return map[string]any{
				"linked": true, "service": service, "username": username,
				"playing": nil, "recent": []track{},
			}, nil
		}
		if err := saveSnapshot(c, db, userID, snap); err != nil {
			return nil, err
		}
		return profilePayload(service, username, snap, time.Now()), nil
	case err != nil:
		return nil, err
	}

	if expired {
		cl.refreshInBackground(db, userID, service, username)
	}

	snap := &snapshot{Recent: []track{}}
	if len(playingRaw) > 0 {
		// 壊れた JSON で表示ごと落とさない。
		_ = json.Unmarshal(playingRaw, &snap.Playing)
	}
	if len(recentRaw) > 0 {
		_ = json.Unmarshal(recentRaw, &snap.Recent)
	}
	return profilePayload(service, username, snap, fetchedAt), nil
}

func profilePayload(service, username string, snap *snapshot, fetchedAt time.Time) map[string]any {
	recent := snap.Recent
	if recent == nil {
		recent = []track{}
	}
	return map[string]any{
		"linked":    true,
		"service":   service,
		"username":  username,
		"playing":   snap.Playing,
		"recent":    recent,
		"fetchedAt": fetchedAt,
	}
}

// refreshInBackground re-fetches without making the caller wait.
func (c *client) refreshInBackground(db *sql.DB, userID, service, username string) {
	if !c.guard.enter(userID) {
		return
	}
	go func() {
		defer c.guard.leave(userID)
		// **リクエストの context を引き継がない。** 呼び出し元の応答が返った
		// 時点で cancel されるので、途中で打ち切られてしまう。
		ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
		defer cancel()

		snap, err := c.fetch(ctx, service, username)
		if err != nil {
			// 取れなくても手元のものは残す。失敗のたびに表示が消えると、
			// 取得元が不調な間ずっと空になる。
			return
		}
		_ = saveSnapshot(ctx, db, userID, snap)
	}()
}

// backgroundTimeout bounds one background refresh.
const backgroundTimeout = 30 * time.Second
