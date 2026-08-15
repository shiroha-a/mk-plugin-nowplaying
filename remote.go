package nowplaying

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shiroha-a/mk/plugin"
)

/*
 * リモート利用者の再生中を、相手のインスタンスから取り寄せる。
 *
 * 経路は mk-go 同士に閉じた peer channel (mk-go #2537)。**問い合わせは非同期**
 * なので、初めて開いたときは出ない。
 */

// remoteTTL is how long a fetched remote profile is reused.
//
// 相手も 30 秒キャッシュを持っているので、それより短く聞いても新しくならない。
// 再生中は変わるが、**相手に負荷をかけない**方を優先する。
const remoteTTL = 2 * time.Minute

// remoteNegativeTTL is how long "that user has not linked anything" is kept.
const remoteNegativeTTL = 10 * time.Minute

// peerRequest is what we ask another instance.
//
// **username だけを送る。** 誰が見に来たかは送らない。
type peerRequest struct {
	Username string `json:"username"`
}

type peerResponse struct {
	Linked  bool            `json:"linked"`
	Profile json.RawMessage `json:"profile,omitempty"`
}

// registerPeer wires both directions of the plugin channel.
func registerPeer(ctx plugin.Context, db *sql.DB, cl *client) {
	peer := ctx.Peer()

	peer.Handle(func(c context.Context, from string, payload json.RawMessage) (any, error) {
		var req peerRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("リクエストを読めません: %w", err)
		}
		if req.Username == "" {
			return nil, errors.New("username が空です")
		}

		userID, err := localUserIDByUsername(c, ctx, req.Username)
		if err != nil {
			return nil, err
		}
		if userID == "" {
			return peerResponse{Linked: false}, nil
		}

		profile, err := buildProfile(c, db, cl, userID)
		if err != nil {
			return nil, err
		}
		if profile == nil {
			return peerResponse{Linked: false}, nil
		}
		body, err := json.Marshal(profile)
		if err != nil {
			return nil, err
		}
		return peerResponse{Linked: true, Profile: body}, nil
	})

	peer.OnReply(func(c context.Context, from, id string, reply json.RawMessage) error {
		var res peerResponse
		if err := json.Unmarshal(reply, &res); err != nil {
			return fmt.Errorf("応答を読めません: %w", err)
		}
		username, err := pendingUsername(c, db, id)
		if err != nil {
			return err
		}
		if username == "" {
			// どの問い合わせの答えか分からない。**捨てる。**
			ctx.Logger().Warn("対応する問い合わせが無い応答を捨てました", "from", from, "id", id)
			return nil
		}
		return saveRemote(c, db, from, username, res)
	})
}

// localUserIDByUsername resolves a local username to its user id.
//
// **mk-go の API を通す。** 凍結や可視性の判断を自前で実装しないため。
// ここは匿名でよい (ゲートが効くのはリモート利用者を引くときだけ、#2106)。
func localUserIDByUsername(c context.Context, ctx plugin.Context, username string) (string, error) {
	api := ctx.API()
	if api == nil {
		return "", nil
	}
	raw, err := api.Anonymous().Call(c, "users/show", map[string]any{"username": username})
	if err != nil {
		var ae *plugin.APIError
		if errors.As(err, &ae) && ae.Status == 404 {
			return "", nil
		}
		return "", err
	}
	var user struct {
		ID   string  `json:"id"`
		Host *string `json:"host"`
	}
	if err := json.Unmarshal(raw, &user); err != nil {
		return "", err
	}
	// **自分のところの利用者だけ答える。** 又貸しすると出どころが分からなくなる。
	if user.Host != nil && *user.Host != "" {
		return "", nil
	}
	return user.ID, nil
}

func rememberPending(c context.Context, db *sql.DB, id, host, username string) error {
	_, err := db.ExecContext(c, `
		INSERT INTO remote_pending (id, host, username, created_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (id) DO NOTHING
	`, id, host, username)
	return err
}

func pendingUsername(c context.Context, db *sql.DB, id string) (string, error) {
	var username string
	err := db.QueryRowContext(c, `SELECT username FROM remote_pending WHERE id = $1`, id).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// 一度使ったら消す。応答は 1 回しか来ない。
	_, _ = db.ExecContext(c, `DELETE FROM remote_pending WHERE id = $1`, id)
	return username, nil
}

func saveRemote(c context.Context, db *sql.DB, host, username string, res peerResponse) error {
	ttl := remoteTTL
	payload := res.Profile
	if !res.Linked || len(payload) == 0 {
		ttl = remoteNegativeTTL
		payload = json.RawMessage(`null`)
	}
	_, err := db.ExecContext(c, `
		INSERT INTO remote_snapshots (host, username, payload, fetched_at, expires_at)
		VALUES ($1, $2, $3, now(), now() + make_interval(secs => $4))
		ON CONFLICT (host, username) DO UPDATE SET
			payload = EXCLUDED.payload, fetched_at = EXCLUDED.fetched_at,
			expires_at = EXCLUDED.expires_at
	`, host, username, []byte(payload), int(ttl.Seconds()))
	return err
}

func remoteProfile(c context.Context, db *sql.DB, host, username string) (json.RawMessage, bool, error) {
	var payload []byte
	var expired bool
	err := db.QueryRowContext(c, `
		SELECT payload, expires_at <= now() FROM remote_snapshots
		WHERE host = $1 AND username = $2
	`, host, username).Scan(&payload, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if string(payload) == "null" {
		return nil, expired, nil
	}
	return payload, expired, nil
}

// remoteLookup answers for a user that is not ours.
func remoteLookup(c context.Context, ctx plugin.Context, db *sql.DB, viewerID, userID string) (any, error) {
	host, username, err := remoteAcct(c, ctx, viewerID, userID)
	if err != nil || host == "" {
		// **エラーにしない。** 音楽と関係のない利用者を開いただけかもしれない。
		return map[string]any{"linked": false}, nil
	}

	cached, stale, err := remoteProfile(c, db, host, username)
	if err != nil {
		return nil, err
	}
	if stale {
		// 期限切れでも**古いものは返す**。取り直しは裏で進む。
		ask(c, ctx, db, host, username)
	}
	if len(cached) == 0 {
		return map[string]any{"linked": false}, nil
	}

	var profile map[string]any
	if err := json.Unmarshal(cached, &profile); err != nil {
		return map[string]any{"linked": false}, nil
	}
	// 相手が返したアート URL は**相手のインスタンスの**プロキシを指す。
	// そのまま出すと CSP で表示できないうえ、閲覧者の接続先が相手に漏れる。
	rewriteArtHosts(profile)
	return profile, nil
}

func ask(c context.Context, ctx plugin.Context, db *sql.DB, host, username string) {
	peer := ctx.Peer()
	ok, err := peer.Has(c, host)
	if err != nil || !ok {
		// 相手が同じプラグインを持っていない。**普通のこと**なので黙って諦める。
		return
	}
	id, err := peer.Send(c, host, peerRequest{Username: username})
	if err != nil {
		ctx.Logger().Debug("リモートへの問い合わせを出せませんでした", "host", host, "err", err)
		return
	}
	if err := rememberPending(c, db, id, host, username); err != nil {
		ctx.Logger().Warn("問い合わせの記録に失敗しました", "id", id, "err", err)
	}
}

// remoteAcct resolves a user id to its host and username.
//
// **閲覧者として引く。** 匿名で引くと `ugcVisibilityForVisitor` が `local`
// (既定) のインスタンスでリモート利用者が NO_SUCH_USER になる (mk-go #2106)。
func remoteAcct(c context.Context, ctx plugin.Context, viewerID, userID string) (host, username string, err error) {
	api := ctx.API()
	if api == nil {
		return "", "", nil
	}
	caller := api.Anonymous()
	if viewerID != "" {
		caller = api.AsUser(viewerID)
	}
	raw, err := caller.Call(c, "users/show", map[string]any{"userId": userID})
	if err != nil {
		return "", "", err
	}
	var user struct {
		Username string  `json:"username"`
		Host     *string `json:"host"`
	}
	if err := json.Unmarshal(raw, &user); err != nil {
		return "", "", err
	}
	if user.Host == nil || *user.Host == "" {
		return "", "", nil
	}
	return *user.Host, user.Username, nil
}

// rewriteArtHosts points artwork URLs at our own proxy.
func rewriteArtHosts(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				t[k] = rewriteArtURL(s)
				continue
			}
			rewriteArtHosts(val)
		}
	case []any:
		for _, val := range t {
			rewriteArtHosts(val)
		}
	}
}

// rewriteArtURL keeps only the key part of a peer's proxy URL.
//
// **相手の URL をそのまま使わない。** 鍵だけを取り出して自分の URL に組み直す
// (鍵の形は検証されるので、任意の取得先には化けない)。
func rewriteArtURL(s string) string {
	i := strings.Index(s, artRoutePrefix)
	if i < 0 {
		return s
	}
	key := s[i+len(artRoutePrefix):]
	if !validArtKey(key) {
		return ""
	}
	return artRoutePrefix + key
}

// validArtKey reports whether a proxy key is one we would fetch.
func validArtKey(key string) bool {
	if name, ok := strings.CutPrefix(key, "mb:"); ok {
		return mbidPattern.MatchString(name)
	}
	if name, ok := strings.CutPrefix(key, "lf:"); ok {
		return lastFmImagePattern.MatchString(name)
	}
	return false
}
