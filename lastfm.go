package nowplaying

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

/*
 * Last.fm (https://www.last.fm/)。
 *
 * ListenBrainz と違って **インスタンスに API キーが要る** (利用者ごとではなく
 * サーバーに 1 つ)。キーが設定されていなければ、そもそも選択肢に出さない。
 *
 * 再生中と履歴が同じエンドポイントから返る。先頭の要素に `@attr.nowplaying`
 * が付いていればそれが再生中。
 */

// lfImage is one of the sizes Last.fm offers.
type lfImage struct {
	Size string `json:"size"`
	Text string `json:"#text"`
}

// lfTrack is one entry of user.getRecentTracks.
//
// **`#text` に本体が入る形が随所にある** (JSON なのに XML の名残)。
type lfTrack struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Artist struct {
		Text string `json:"#text"`
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Text string `json:"#text"`
	} `json:"album"`
	Image []lfImage `json:"image"`
	Date  struct {
		UTS string `json:"uts"`
	} `json:"date"`
	Attr struct {
		NowPlaying string `json:"nowplaying"`
	} `json:"@attr"`
}

func (t lfTrack) artist() string {
	if t.Artist.Text != "" {
		return t.Artist.Text
	}
	return t.Artist.Name
}

// bestImage picks the largest artwork Last.fm offers.
//
// 並び順は small → medium → large → extralarge だが、**順序に頼らない**。
// 欲しい size を名前で探し、無ければ最後のものを使う。
func (t lfTrack) bestImage() string {
	for _, want := range []string{"extralarge", "large", "medium"} {
		for _, img := range t.Image {
			if img.Size == want && img.Text != "" {
				return img.Text
			}
		}
	}
	for i := len(t.Image) - 1; i >= 0; i-- {
		if t.Image[i].Text != "" {
			return t.Image[i].Text
		}
	}
	return ""
}

func (t lfTrack) toTrack() track {
	out := track{
		Title:  t.Name,
		Artist: t.artist(),
		Album:  t.Album.Text,
		Art:    artURL(artKeyForLastFm(t.bestImage())),
		URL:    t.URL,
	}
	if t.Attr.NowPlaying != "true" {
		// 再生中のものに再生時刻は無い。付いているものだけ拾う。
		if ts, err := strconv.ParseInt(t.Date.UTS, 10, 64); err == nil {
			out.ListenedAt = ts
		}
	}
	return out
}

// lfResponse is the shape of user.getRecentTracks.
//
// **`track` は 1 件のときオブジェクトになる。** 配列で決め打つと、聴取が 1 件
// しかない利用者で丸ごと壊れる。生のまま受けて両方に対応する。
type lfResponse struct {
	RecentTracks struct {
		Track json.RawMessage `json:"track"`
	} `json:"recenttracks"`
	// Last.fm はエラーも 200 で返すことがある。
	Error   int    `json:"error"`
	Message string `json:"message"`
}

// tracks normalises the one-or-many shape.
func (r lfResponse) tracks() ([]lfTrack, error) {
	raw := r.RecentTracks.Track
	if len(raw) == 0 {
		return nil, nil
	}
	var many []lfTrack
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one lfTrack
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []lfTrack{one}, nil
}

// lastFmErrNoUser is what Last.fm returns for an unknown account.
const lastFmErrNoUser = 6

func (c *client) fetchLastFm(ctx context.Context, username string) (*snapshot, error) {
	q := url.Values{}
	q.Set("method", "user.getrecenttracks")
	q.Set("user", username)
	q.Set("api_key", c.set.LastFmAPIKey)
	q.Set("format", "json")
	// 再生中の 1 件が先頭に挟まるので、履歴を maxRecent 件確保するには 1 多く要る。
	q.Set("limit", strconv.Itoa(maxRecent+1))

	res, err := getJSON[lfResponse](ctx, c, c.set.LastFmEndpoint+"/2.0/?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// **200 でもエラーが返る。** ステータスだけ見ていると、存在しない利用者を
	// 「聴取なし」として保存してしまう。
	if res.Error != 0 {
		if res.Error == lastFmErrNoUser {
			return nil, &upstreamError{status: 404,
				userFacing: "そのユーザー名が見つかりません", msg: "lastfm: error 6"}
		}
		return nil, &upstreamError{status: 502, msg: "lastfm: error " + strconv.Itoa(res.Error)}
	}

	list, err := res.tracks()
	if err != nil {
		return nil, err
	}

	snap := &snapshot{}
	for _, t := range list {
		conv := t.toTrack()
		if t.Attr.NowPlaying == "true" && snap.Playing == nil {
			snap.Playing = &conv
			continue
		}
		if len(snap.Recent) >= maxRecent {
			break
		}
		snap.Recent = append(snap.Recent, conv)
	}
	return snap, nil
}
