package nowplaying

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lfServer serves user.getRecentTracks.
func lfServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "" {
			t.Error("api_key を送っていない")
		}
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format=json を送っていない: %q", r.URL.Query().Get("format"))
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func lastFmClient(t *testing.T, endpoint string) *client {
	t.Helper()
	c := testClient(t, endpoint)
	c.set.LastFmAPIKey = "test-key"
	return c
}

const lfTracks = `{"recenttracks":{"track":[
	{"artist":{"#text":"Röyksopp"},"name":"What Else Is There?","album":{"#text":"The Understanding"},
	 "image":[{"size":"small","#text":"https://lastfm.freetls.fastly.net/i/u/34s/0123456789abcdef0123456789abcdef.png"},
	          {"size":"extralarge","#text":"https://lastfm.freetls.fastly.net/i/u/300x300/0123456789abcdef0123456789abcdef.png"}],
	 "url":"https://www.last.fm/music/x","@attr":{"nowplaying":"true"}},
	{"artist":{"#text":"Aphex Twin"},"name":"Xtal","album":{"#text":"SAW 85-92"},
	 "image":[{"size":"extralarge","#text":"https://lastfm.freetls.fastly.net/i/u/300x300/fedcba9876543210fedcba9876543210.jpg"}],
	 "date":{"uts":"1771414109"}}
]}}`

func TestLastFm_ParsesPlayingAndRecent(t *testing.T) {
	srv := lfServer(t, lfTracks)
	got, err := lastFmClient(t, srv.URL).fetch(context.Background(), serviceLastFm, "someone")
	if err != nil {
		t.Fatal(err)
	}

	if got.Playing == nil {
		t.Fatal("再生中が拾えていない")
	}
	if got.Playing.Title != "What Else Is There?" || got.Playing.Artist != "Röyksopp" {
		t.Errorf("再生中: %+v", got.Playing)
	}
	// **再生中は履歴に混ぜない。** 混ざると同じ曲が 2 度並ぶ。
	if len(got.Recent) != 1 || got.Recent[0].Title != "Xtal" {
		t.Errorf("履歴: %+v", got.Recent)
	}
	if got.Recent[0].ListenedAt != 1771414109 {
		t.Errorf("再生時刻: %d", got.Recent[0].ListenedAt)
	}
	if !strings.HasPrefix(got.Playing.Art, artRoutePrefix+"lf:") {
		t.Errorf("アートが proxy 経由でない: %q", got.Playing.Art)
	}
}

// **`track` は 1 件のときオブジェクトで返る。** 配列で決め打つと、聴取が
// 1 件しかない利用者で丸ごと壊れる。
func TestLastFm_SingleTrackIsObject(t *testing.T) {
	body := `{"recenttracks":{"track":
		{"artist":{"#text":"Boards of Canada"},"name":"Roygbiv","album":{"#text":"MHTRTC"},
		 "date":{"uts":"1771414000"}}}}`
	srv := lfServer(t, body)

	got, err := lastFmClient(t, srv.URL).fetch(context.Background(), serviceLastFm, "someone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) != 1 || got.Recent[0].Title != "Roygbiv" {
		t.Fatalf("1 件だけの応答を読めていない: %+v", got.Recent)
	}
}

// **200 でもエラーが返る。** ステータスだけ見ていると、存在しない利用者を
// 「聴取なし」として保存してしまう。
func TestLastFm_ErrorInBody(t *testing.T) {
	srv := lfServer(t, `{"error":6,"message":"User not found"}`)
	_, err := lastFmClient(t, srv.URL).fetch(context.Background(), serviceLastFm, "someone")
	if err == nil {
		t.Fatal("200 で返るエラーを見逃している")
	}
	ue, ok := err.(*upstreamError)
	if !ok || ue.userFacing == "" {
		t.Fatalf("利用者に理由が伝わらない: %v", err)
	}

	// 6 以外は利用者の責任ではないので見せない。
	srv2 := lfServer(t, `{"error":29,"message":"Rate limit exceeded"}`)
	_, err = lastFmClient(t, srv2.URL).fetch(context.Background(), serviceLastFm, "someone")
	ue2, ok := err.(*upstreamError)
	if !ok || ue2.userFacing != "" {
		t.Errorf("こちらの都合でない失敗を利用者に見せている: %v", err)
	}
}

// 画像は大きいものを選ぶ。並び順に頼らず size 名で探す。
func TestLastFm_PicksLargestImage(t *testing.T) {
	tr := lfTrack{Image: []lfImage{
		{Size: "small", Text: "https://x/i/u/34s/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"},
		{Size: "extralarge", Text: "https://x/i/u/300x300/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"},
		{Size: "medium", Text: "https://x/i/u/64s/cccccccccccccccccccccccccccccccc.png"},
	}}
	if !strings.Contains(tr.bestImage(), "bbbb") {
		t.Errorf("大きい画像を選んでいない: %q", tr.bestImage())
	}

	// size 名が付いていなければ最後のものを使う。
	tr2 := lfTrack{Image: []lfImage{{Text: "https://x/i/u/34s/dddddddddddddddddddddddddddddddd.png"}}}
	if tr2.bestImage() == "" {
		t.Error("size 名が無いと諦めている")
	}
	if (lfTrack{}).bestImage() != "" {
		t.Error("画像が無いのに値を返した")
	}
}

// アーティスト名の入り方が 2 通りある。
func TestLastFm_ArtistFallback(t *testing.T) {
	var t1 lfTrack
	t1.Artist.Text = "from-text"
	t1.Artist.Name = "from-name"
	if t1.artist() != "from-text" {
		t.Errorf("#text を優先していない: %q", t1.artist())
	}

	var t2 lfTrack
	t2.Artist.Name = "from-name"
	if t2.artist() != "from-name" {
		t.Errorf("name に落ちていない: %q", t2.artist())
	}
}

// track が想定外の形 (数値など) でも落とさない。
func TestLastFm_BrokenTrackShape(t *testing.T) {
	srv := lfServer(t, `{"recenttracks":{"track":42}}`)
	if _, err := lastFmClient(t, srv.URL).fetch(context.Background(), serviceLastFm, "someone"); err == nil {
		t.Fatal("読めない形を通している")
	}

	// 空でもエラーにはしない (まだ何も聴いていないだけ)。
	srv2 := lfServer(t, `{"recenttracks":{}}`)
	got, err := lastFmClient(t, srv2.URL).fetch(context.Background(), serviceLastFm, "someone")
	if err != nil {
		t.Fatalf("空の履歴をエラーにしている: %v", err)
	}
	if got.Playing != nil || len(got.Recent) != 0 {
		t.Errorf("空でない: %+v", got)
	}
}
