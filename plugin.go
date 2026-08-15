// Package nowplaying shows what a user is listening to on their Misskey
// profile.
//
// 取得元は ListenBrainz (認証不要の公開 API) と Last.fm (インスタンスに API
// キーが要る) の 2 つ。利用者がどちらかを選んでユーザー名を登録する。
package nowplaying

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/shiroha-a/mk/plugin"
)

// Plugin is the entry point referenced by the generated registration code.
var Plugin = plugin.Definition{
	Name:       "nowplaying",
	Version:    "0.1.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
	// 同じプラグインを入れた mk-go 同士で、リモート利用者の再生中を取り寄せる
	// (mk-go #2537)。
	Peered: true,
}

// settings mirrors the `plugins.nowplaying` section of the instance config.
type settings struct {
	// ListenBrainzEndpoint is the ListenBrainz API base URL.
	ListenBrainzEndpoint string `json:"listenBrainzEndpoint"`
	// LastFmEndpoint is the Last.fm API base URL.
	LastFmEndpoint string `json:"lastFmEndpoint"`
	// LastFmAPIKey enables the Last.fm side.
	//
	// **キーが無ければ Last.fm は選べない。** 利用者が選んでから「使えません」と
	// 言うより、最初から出さない方がよい。
	LastFmAPIKey string `json:"lastFmApiKey"`
	// CoverArtEndpoint is the Cover Art Archive base URL.
	CoverArtEndpoint string `json:"coverArtEndpoint"`
	// UserAgent identifies this instance upstream.
	//
	// MusicBrainz 系は User-Agent を明示的に求めている (無名のリクエストは
	// 弾かれることがある)。
	UserAgent string `json:"userAgent"`
	// TimeoutSeconds bounds one upstream request.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

func loadSettings(ctx plugin.Context) (settings, error) {
	s := settings{
		ListenBrainzEndpoint: "https://api.listenbrainz.org",
		LastFmEndpoint:       "https://ws.audioscrobbler.com",
		CoverArtEndpoint:     "https://coverartarchive.org",
		UserAgent:            "mk-go-plugin-nowplaying/0.1 (+https://github.com/shiroha-a/mk)",
		TimeoutSeconds:       10,
	}
	if err := ctx.Config().Unmarshal(&s); err != nil {
		return s, err
	}
	return s, nil
}

var migrations = []plugin.Migration{
	{Version: 1, SQL: `
		CREATE TABLE accounts (
			user_id    text PRIMARY KEY,
			service    text NOT NULL,
			username   text NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE snapshots (
			user_id    text PRIMARY KEY,
			playing    jsonb NOT NULL DEFAULT 'null',
			recent     jsonb NOT NULL DEFAULT '[]',
			fetched_at timestamptz NOT NULL DEFAULT now(),
			expires_at timestamptz NOT NULL
		);
		CREATE TABLE remote_snapshots (
			host       text NOT NULL,
			username   text NOT NULL,
			payload    jsonb NOT NULL,
			fetched_at timestamptz NOT NULL DEFAULT now(),
			expires_at timestamptz NOT NULL,
			PRIMARY KEY (host, username)
		);
		CREATE TABLE remote_pending (
			id         text PRIMARY KEY,
			host       text NOT NULL,
			username   text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now()
		);
	`},
}

// serviceListenBrainz / serviceLastFm are the supported sources.
const (
	serviceListenBrainz = "listenbrainz"
	serviceLastFm       = "lastfm"
)

// usernamePattern restricts what we will put in an upstream URL.
//
// **利用者が入れた文字列がそのまま取得先 URL の一部になる。** 経路を変えられ
// ないよう、両サービスが許す文字だけを通す。
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// cacheTTL is how long a fetched snapshot is reused.
//
// **原神系とは考え方が違う。** あちらは取得元が ttl を指定するので待てばよい
// が、再生中の曲は数分で変わるので鮮度が要る。かといって開くたびに外へ出ると
// 相手に負荷をかけるので、短いキャッシュで折り合う。
const cacheTTL = 30 * time.Second

func routes(ctx plugin.Context, r plugin.Router) error {
	set, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()
	client := newClient(set)

	registerPeer(ctx, db, client)

	r.POST("/me", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		var service, username string
		err := db.QueryRowContext(req.Context(),
			`SELECT service, username FROM accounts WHERE user_id = $1`, me).Scan(&service, &username)
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{"service": nil, "username": nil, "lastFmAvailable": set.LastFmAPIKey != ""}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"service": service, "username": username,
			// Last.fm はインスタンスに API キーが要る。選べるかどうかを
			// 設定画面に伝える。
			"lastFmAvailable": set.LastFmAPIKey != "",
		}, nil
	})

	r.POST("/me/set", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		var body struct {
			Service  string `json:"service"`
			Username string `json:"username"`
		}
		if err := req.Bind(&body); err != nil {
			return nil, plugin.Errorf(http.StatusBadRequest, "リクエストを読めません")
		}

		// 空文字は登録解除として扱う。UI から消したときに消せないと不便。
		if body.Username == "" {
			for _, q := range []string{
				`DELETE FROM accounts WHERE user_id = $1`,
				`DELETE FROM snapshots WHERE user_id = $1`,
			} {
				if _, err := db.ExecContext(req.Context(), q, me); err != nil {
					return nil, err
				}
			}
			return map[string]any{"service": nil, "username": nil}, nil
		}

		switch body.Service {
		case serviceListenBrainz:
		case serviceLastFm:
			if set.LastFmAPIKey == "" {
				return nil, plugin.Errorf(http.StatusBadRequest,
					"このサーバーでは Last.fm を使えません (API キーが設定されていません)")
			}
		default:
			return nil, plugin.Errorf(http.StatusBadRequest, "対応していないサービスです")
		}
		if !usernamePattern.MatchString(body.Username) {
			return nil, plugin.Errorf(http.StatusBadRequest, "ユーザー名の形式が正しくありません")
		}

		// **登録時に 1 度取得して存在を確かめる。** 綴りを間違えたまま保存すると、
		// 何も出ない理由が利用者に分からない。
		snap, err := client.fetch(req.Context(), body.Service, body.Username)
		if err != nil {
			var ue *upstreamError
			if errors.As(err, &ue) && ue.userFacing != "" {
				return nil, plugin.Errorf(ue.status, "%s", ue.userFacing)
			}
			// 上流の一時的な不調で登録を拒むと、直るまで設定できない。
			ctx.Logger().Warn("登録時の取得に失敗しました (保存は行います)", "err", err)
		} else if err := saveSnapshot(req.Context(), db, me, snap); err != nil {
			return nil, err
		}

		if _, err := db.ExecContext(req.Context(), `
			INSERT INTO accounts (user_id, service, username, updated_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (user_id) DO UPDATE SET
				service = EXCLUDED.service, username = EXCLUDED.username, updated_at = now()
		`, me, body.Service, body.Username); err != nil {
			return nil, err
		}
		return map[string]any{"service": body.Service, "username": body.Username}, nil
	})

	r.POST("/profile", func(req plugin.Request) (any, error) {
		var body struct {
			UserID string `json:"userId"`
		}
		if err := req.Bind(&body); err != nil || body.UserID == "" {
			return nil, plugin.Errorf(http.StatusBadRequest, "userId が必要です")
		}

		profile, err := buildProfile(req.Context(), db, client, body.UserID)
		if err != nil {
			return nil, err
		}
		if profile != nil {
			return profile, nil
		}
		return remoteLookup(req.Context(), ctx, db, req.UserID(), body.UserID)
	})

	// アルバムアートの中継。本体の CSP は `img-src 'self'` なので、外部の
	// 画像を <img> で直接読めない。
	r.GET("/art/:key", func(req plugin.Request) (any, error) {
		body, err := client.art(req.Context(), req.Param("key"))
		if err != nil {
			var ue *upstreamError
			if errors.As(err, &ue) && ue.status == http.StatusBadRequest {
				return nil, plugin.Errorf(http.StatusBadRequest, "アートの指定が不正です")
			}
			return nil, plugin.ErrNotFound("アートが見つかりません")
		}
		return plugin.Blob{
			ContentType: body.contentType,
			Body:        body.data,
			// ジャケットは変わらないので長めに持たせる。
			CacheControl: "public, max-age=86400, immutable",
		}, nil
	})

	return nil
}

func jobs(ctx plugin.Context, j plugin.Jobs) error {
	db := ctx.Storage().DB()

	// **定期取得はしない。** 再生中は数分で変わるので、先回りして取っても
	// 見られる頃には古い。プロフィールを開いたときに取る方が当たる。
	// ここでやるのは、聴かなくなった人の行を溜め込まないための掃除だけ。
	j.Handle("sweep", func(c context.Context, _ json.RawMessage) error {
		_, err := db.ExecContext(c, `
			DELETE FROM snapshots WHERE fetched_at < now() - interval '30 days'
		`)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(c, `
			DELETE FROM remote_snapshots WHERE fetched_at < now() - interval '7 days'
		`)
		if err != nil {
			return err
		}
		// 応答が返らなかった問い合わせの記録も残り続けるので落とす。
		_, err = db.ExecContext(c, `
			DELETE FROM remote_pending WHERE created_at < now() - interval '1 day'
		`)
		return err
	})
	j.Schedule("17 4 * * *", "sweep", nil)
	return nil
}

func saveSnapshot(c context.Context, db *sql.DB, userID string, s *snapshot) error {
	playing, err := json.Marshal(s.Playing)
	if err != nil {
		return err
	}
	recent, err := json.Marshal(s.Recent)
	if err != nil {
		return err
	}
	if s.Recent == nil {
		recent = []byte("[]")
	}
	_, err = db.ExecContext(c, `
		INSERT INTO snapshots (user_id, playing, recent, fetched_at, expires_at)
		VALUES ($1, $2, $3, now(), now() + make_interval(secs => $4))
		ON CONFLICT (user_id) DO UPDATE SET
			playing = EXCLUDED.playing, recent = EXCLUDED.recent,
			fetched_at = EXCLUDED.fetched_at, expires_at = EXCLUDED.expires_at
	`, userID, playing, recent, int(cacheTTL.Seconds()))
	return err
}

// --- 取得元 ---

// track is one listened track, normalised across services.
type track struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album,omitempty"`
	// Art is the proxied cover URL, empty when we could not resolve one.
	Art string `json:"art,omitempty"`
	// ListenedAt is a unix timestamp. 再生中のものは 0。
	ListenedAt int64 `json:"listenedAt,omitempty"`
	// URL points at the track on the source service.
	URL string `json:"url,omitempty"`
}

type snapshot struct {
	// Playing is the currently playing track, nil when nothing is.
	Playing *track `json:"playing"`
	// Recent holds the latest listens, newest first.
	Recent []track `json:"recent"`
}

type upstreamError struct {
	status int
	// userFacing is non-empty when the failure is the user's fault and should
	// be shown to them (no such account).
	userFacing string
	msg        string
}

func (e *upstreamError) Error() string { return e.msg }

type client struct {
	set  settings
	http *http.Client
	// artHTTP follows redirects, but only to hosts we allow.
	artHTTP *http.Client
	arts    *artCache
	// guard keeps background refreshes from piling up.
	guard *refreshGuard
}

func newClient(set settings) *client {
	timeout := time.Duration(set.TimeoutSeconds) * time.Second
	return &client{
		set:     set,
		http:    &http.Client{Timeout: timeout},
		artHTTP: newArtHTTPClient(timeout),
		arts:    newArtCache(artCacheLimit),
		guard:   newRefreshGuard(),
	}
}

// fetch retrieves the snapshot for one account.
func (c *client) fetch(ctx context.Context, service, username string) (*snapshot, error) {
	if !usernamePattern.MatchString(username) {
		return nil, &upstreamError{status: http.StatusBadRequest, msg: "ユーザー名の形式が不正です"}
	}
	switch service {
	case serviceListenBrainz:
		return c.fetchListenBrainz(ctx, username)
	case serviceLastFm:
		return c.fetchLastFm(ctx, username)
	default:
		return nil, &upstreamError{status: http.StatusBadRequest, msg: "対応していないサービスです"}
	}
}

// maxRecent bounds how many past listens we keep.
//
// 表示に使うのは数件。取得元が想定外の数を返しても保存が膨らまないよう切る。
const maxRecent = 10
