package nowplaying

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
 * ジャケット画像を同一オリジンで中継する。
 *
 * 本体の CSP は `img-src 'self'` なので、取得元の URL を <img> にそのまま
 * 渡しても表示できない。
 *
 * # 任意の URL を中継しないこと
 *
 * ここは**外部から与えられた文字列で外向きのリクエストを出す**場所なので、
 * SSRF の入口になりうる。取得元の応答に入っている URL をそのまま使わず、
 *
 *   - MusicBrainz の release id (UUID)
 *   - Last.fm の画像ハッシュ (32 桁の hex)
 *
 * だけを鍵として受け取り、**URL はこちらで組み立てる**。リダイレクト先も
 * ホストを検査する (Cover Art Archive は Internet Archive の実体サーバーへ
 * 飛ばすので、リダイレクト自体は許さざるを得ない)。
 */

// artRoutePrefix is where this plugin serves proxied artwork.
const artRoutePrefix = "/api/plugin/nowplaying/art/"

// artCacheLimit bounds what the process keeps.
const artCacheLimit = 32 << 20

// maxArtBytes bounds one download.
const maxArtBytes = 4 << 20

// coverArtSize is the thumbnail we ask Cover Art Archive for.
//
// 原寸は数 MB あることがある。表示は 48px 程度なので 250 で足りる。
const coverArtSize = "front-250"

// lastFmSize is the variant we ask Last.fm for.
const lastFmSize = "300x300"

var (
	// mbidPattern matches a MusicBrainz identifier.
	mbidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// lastFmImagePattern matches the file part of a Last.fm image URL.
	lastFmImagePattern = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|jpeg|gif|webp)$`)
)

// lastFmNoImage is the placeholder Last.fm returns when a track has no art.
//
// これを中継しても灰色の四角が出るだけなので、アートが無いものとして扱う。
const lastFmNoImage = "2a96cbd8b46e442fc41c2b86b821562f"

// artKeyForMBID builds the proxy key for a MusicBrainz release.
func artKeyForMBID(mbid string) string {
	if !mbidPattern.MatchString(mbid) {
		return ""
	}
	return "mb:" + mbid
}

// artKeyForLastFm builds the proxy key from a Last.fm image URL.
//
// **URL 全体は持ち回らない。** ファイル名だけを取り出して、URL はこちらで
// 組み直す。
func artKeyForLastFm(imageURL string) string {
	if imageURL == "" {
		return ""
	}
	file := imageURL[strings.LastIndex(imageURL, "/")+1:]
	if !lastFmImagePattern.MatchString(file) {
		return ""
	}
	if strings.HasPrefix(file, lastFmNoImage) {
		return ""
	}
	return "lf:" + file
}

// artURL turns a key into the same-origin proxy URL.
func artURL(key string) string {
	if key == "" {
		return ""
	}
	return artRoutePrefix + key
}

// artSource resolves a proxy key back to the URL to fetch.
func (c *client) artSource(key string) (string, error) {
	name, ok := strings.CutPrefix(key, "mb:")
	if ok {
		if !mbidPattern.MatchString(name) {
			return "", &upstreamError{status: http.StatusBadRequest, msg: "mbid が不正です"}
		}
		return c.set.CoverArtEndpoint + "/release/" + name + "/" + coverArtSize, nil
	}
	if name, ok := strings.CutPrefix(key, "lf:"); ok {
		if !lastFmImagePattern.MatchString(name) {
			return "", &upstreamError{status: http.StatusBadRequest, msg: "画像名が不正です"}
		}
		return "https://lastfm.freetls.fastly.net/i/u/" + lastFmSize + "/" + name, nil
	}
	return "", &upstreamError{status: http.StatusBadRequest, msg: "鍵の形式が不正です"}
}

// allowedArtHost reports whether we may follow a redirect to host.
//
// # Internet Archive のノード名を列挙しないこと
//
// Cover Art Archive は実体を Internet Archive に置いており、そこからさらに
// 実サーバーへ飛ばす。**このホスト名の形は 1 つではない。**
//
//	coverartarchive.org → archive.org → dn710006.ca.archive.org
//	                                  → ia801404.us.archive.org
//
// 最初に見た `ia<数字>.us.archive.org` だけを許していたら、`dn<数字>.ca.` の
// 系統で弾かれてジャケットが出なかった (実運用で踏んだ)。ノードは増減するので
// 個別に数えず、**archive.org のサブドメインをまとめて許す**。
//
// 末尾一致ではなく `.archive.org` で終わることを見る点が要で、`HasSuffix` に
// 先頭のドットを含めないと `archive.org.evil.example` が通ってしまう。
func allowedArtHost(host string) bool {
	switch host {
	case "coverartarchive.org", "lastfm.freetls.fastly.net", "archive.org":
		return true
	}
	return strings.HasSuffix(host, ".archive.org")
}

// newArtHTTPClient follows redirects, but only within hosts we allow.
//
// Cover Art Archive は実体を Internet Archive に置いているので、リダイレクトを
// 追わないと画像が取れない。**追う先を必ず検査する** — 取得元が乗っ取られた
// ときに、こちらのサーバーから任意のホストへリクエストが出るのを防ぐ。
func newArtHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("リダイレクトが多すぎます")
			}
			// https から降りない。平文に落とされると経路上で差し替えられる。
			if req.URL.Scheme != "https" {
				return fmt.Errorf("https 以外へのリダイレクト: %s", req.URL.Scheme)
			}
			if !allowedArtHost(req.URL.Hostname()) {
				return fmt.Errorf("許可していないホストへのリダイレクト: %s", req.URL.Hostname())
			}
			return nil
		},
	}
}

// artBlob is a fetched image.
type artBlob struct {
	data        []byte
	contentType string
}

// art fetches (and caches) one cover.
func (c *client) art(ctx context.Context, key string) (artBlob, error) {
	src, err := c.artSource(key)
	if err != nil {
		return artBlob{}, err
	}
	if b, ok := c.arts.get(key); ok {
		return b, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return artBlob{}, err
	}
	req.Header.Set("User-Agent", c.set.UserAgent)

	res, err := c.artHTTP.Do(req)
	if err != nil {
		return artBlob{}, err
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て
	if res.StatusCode != http.StatusOK {
		return artBlob{}, &upstreamError{status: res.StatusCode,
			msg: fmt.Sprintf("art: status %d", res.StatusCode)}
	}

	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return artBlob{}, &upstreamError{status: http.StatusBadGateway, msg: "画像ではない応答が返りました"}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxArtBytes))
	if err != nil {
		return artBlob{}, err
	}

	// **取得元が名乗る型をそのまま返さない。** 扱うのは既知の画像形式だけと
	// 決めて、こちらで正規化する。
	out := artBlob{data: body, contentType: normalizeImageType(ct)}
	c.arts.put(key, out)
	return out, nil
}

// normalizeImageType maps an upstream Content-Type onto one we serve.
func normalizeImageType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "image/png"):
		return "image/png"
	case strings.HasPrefix(ct, "image/gif"):
		return "image/gif"
	case strings.HasPrefix(ct, "image/webp"):
		return "image/webp"
	default:
		// Cover Art Archive も Last.fm も JPEG が主。判別できないものは
		// JPEG として返す (画像であることは確認済み)。
		return "image/jpeg"
	}
}

// artCache keeps fetched covers in the process.
//
// **LRU ではなく投入順に捨てる。** 追い出しの精度より、同じジャケットを何度も
// 取りに行かないことの方が効く。
type artCache struct {
	mu    sync.Mutex
	items map[string]artBlob
	order []string
	bytes int
	limit int
}

func newArtCache(limit int) *artCache {
	return &artCache{items: map[string]artBlob{}, limit: limit}
}

func (c *artCache) get(key string) (artBlob, bool) {
	if c == nil {
		return artBlob{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *artCache) put(key string, b artBlob) {
	if c == nil || len(b.data) > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; exists {
		return
	}
	for c.bytes+len(b.data) > c.limit && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.items[oldest].data)
		delete(c.items, oldest)
	}
	c.items[key] = b
	c.order = append(c.order, key)
	c.bytes += len(b.data)
}
