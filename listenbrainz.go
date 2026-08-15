/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

package nowplaying

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

/*
 * ListenBrainz (https://listenbrainz.org/)。
 *
 * **認証が要らない。** 公開されている再生履歴をそのまま読めるので、インスタンス
 * 側の設定なしで使える。Last.fm と違って API キーの用意も要らない。
 *
 * 再生中と履歴が別のエンドポイントに分かれているので 2 回叩く。
 */

// maxUpstreamBytes bounds one upstream response.
const maxUpstreamBytes = 4 << 20

// lbListen is one entry of either endpoint.
type lbListen struct {
	// ListenedAt is absent on the playing-now endpoint.
	ListenedAt    int64 `json:"listened_at"`
	TrackMetadata struct {
		ArtistName  string `json:"artist_name"`
		TrackName   string `json:"track_name"`
		ReleaseName string `json:"release_name"`
		// AdditionalInfo carries the MusicBrainz ids we need for cover art.
		AdditionalInfo struct {
			ReleaseMBID string `json:"release_mbid"`
		} `json:"additional_info"`
		// MBIDMapping is filled in by ListenBrainz when it could match the
		// listen to MusicBrainz. **こちらにしか mbid が無いことがある** —
		// 送信側がタグを付けていない場合、additional_info は空になる。
		MBIDMapping struct {
			ReleaseMBID    string `json:"release_mbid"`
			CAAReleaseMBID string `json:"caa_release_mbid"`
		} `json:"mbid_mapping"`
	} `json:"track_metadata"`
}

// releaseMBID picks whichever release id is available.
func (l lbListen) releaseMBID() string {
	m := l.TrackMetadata
	// Cover Art Archive 用に選ばれた id があればそれが最も確実。
	if v := m.MBIDMapping.CAAReleaseMBID; v != "" {
		return v
	}
	if v := m.AdditionalInfo.ReleaseMBID; v != "" {
		return v
	}
	return m.MBIDMapping.ReleaseMBID
}

func (l lbListen) toTrack() track {
	m := l.TrackMetadata
	return track{
		Title:      m.TrackName,
		Artist:     m.ArtistName,
		Album:      m.ReleaseName,
		Art:        artURL(artKeyForMBID(l.releaseMBID())),
		ListenedAt: l.ListenedAt,
	}
}

type lbPayload struct {
	Payload struct {
		Count   int        `json:"count"`
		Listens []lbListen `json:"listens"`
	} `json:"payload"`
}

func (c *client) fetchListenBrainz(ctx context.Context, username string) (*snapshot, error) {
	base := c.set.ListenBrainzEndpoint + "/1/user/" + url.PathEscape(username)

	// 再生中。**ここが空でも異常ではない** (今は何も聴いていないだけ)。
	nowPayload, err := getJSON[lbPayload](ctx, c, base+"/playing-now")
	if err != nil {
		return nil, err
	}
	snap := &snapshot{}
	if len(nowPayload.Payload.Listens) > 0 {
		t := nowPayload.Payload.Listens[0].toTrack()
		snap.Playing = &t
	}

	// 履歴。
	recent, err := getJSON[lbPayload](ctx, c, fmt.Sprintf("%s/listens?count=%d", base, maxRecent))
	if err != nil {
		return nil, err
	}
	for i, l := range recent.Payload.Listens {
		if i >= maxRecent {
			break
		}
		snap.Recent = append(snap.Recent, l.toTrack())
	}
	return snap, nil
}

// getJSON fetches and decodes one upstream response.
func getJSON[T any](ctx context.Context, c *client, endpoint string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return zero, err
	}
	// MusicBrainz 系は User-Agent を明示的に求めている。
	req.Header.Set("User-Agent", c.set.UserAgent)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("取得元への接続に失敗しました: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // 読み捨て

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return zero, &upstreamError{status: http.StatusNotFound,
			userFacing: "そのユーザー名が見つかりません", msg: "upstream: 404"}
	default:
		// 429 (レート制限) や 5xx。こちらの都合ではないので利用者には見せない。
		return zero, &upstreamError{status: res.StatusCode,
			msg: fmt.Sprintf("upstream: status %d", res.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxUpstreamBytes))
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, fmt.Errorf("取得元の応答を解釈できません: %w", err)
	}
	return out, nil
}
