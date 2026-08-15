package nowplaying

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lbServer serves the two ListenBrainz endpoints.
//
// 形は実際の応答をなぞっている (api.listenbrainz.org を実際に叩いて確認した)。
func lbServer(t *testing.T, now, listens string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent を送っていない (MusicBrainz 系は明示的に求めている)")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/playing-now"):
			_, _ = w.Write([]byte(now))
		case strings.HasSuffix(r.URL.Path, "/listens"):
			_, _ = w.Write([]byte(listens))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClient(t *testing.T, endpoint string) *client {
	t.Helper()
	return newClient(settings{
		ListenBrainzEndpoint: endpoint,
		LastFmEndpoint:       endpoint,
		CoverArtEndpoint:     endpoint,
		UserAgent:            "test/1.0",
		TimeoutSeconds:       5,
	})
}

const lbNowPlaying = `{"payload":{"count":1,"playing_now":true,"listens":[{
	"track_metadata":{
		"artist_name":"Röyksopp","track_name":"What Else Is There?","release_name":"The Understanding",
		"mbid_mapping":{"caa_release_mbid":"f1418001-7f1e-46af-bfdb-95faeded8841"}
	}}]}}`

const lbListens = `{"payload":{"count":2,"listens":[
	{"listened_at":1771414109,"track_metadata":{
		"artist_name":"Boards of Canada","track_name":"Roygbiv","release_name":"Music Has the Right to Children",
		"additional_info":{"release_mbid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}},
	{"listened_at":1771414000,"track_metadata":{
		"artist_name":"Aphex Twin","track_name":"Xtal","release_name":"Selected Ambient Works 85-92"}}
]}}`

func TestListenBrainz_ParsesPlayingAndRecent(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	got, err := testClient(t, srv.URL).fetch(context.Background(), serviceListenBrainz, "someone")
	if err != nil {
		t.Fatal(err)
	}

	if got.Playing == nil {
		t.Fatal("再生中が拾えていない")
	}
	if got.Playing.Title != "What Else Is There?" || got.Playing.Artist != "Röyksopp" {
		t.Errorf("再生中: %+v", got.Playing)
	}
	// **再生中に再生時刻は無い。** あるように見せると「たった今」が出続ける。
	if got.Playing.ListenedAt != 0 {
		t.Errorf("再生中に時刻が入っている: %d", got.Playing.ListenedAt)
	}
	// ジャケットは自分のプロキシ経由 (CSP が img-src 'self')。
	if !strings.HasPrefix(got.Playing.Art, artRoutePrefix) {
		t.Errorf("アートが proxy 経由でない: %q", got.Playing.Art)
	}

	if len(got.Recent) != 2 {
		t.Fatalf("履歴の数: %d", len(got.Recent))
	}
	if got.Recent[0].Title != "Roygbiv" || got.Recent[0].ListenedAt != 1771414109 {
		t.Errorf("履歴の先頭: %+v", got.Recent[0])
	}
	// mbid が無いものはアートを出さない (URL を捏造しない)。
	if got.Recent[1].Art != "" {
		t.Errorf("mbid が無いのにアートを付けた: %q", got.Recent[1].Art)
	}
}

// 何も聴いていない状態は**異常ではない**。
func TestListenBrainz_NothingPlaying(t *testing.T) {
	srv := lbServer(t, `{"payload":{"count":0,"playing_now":true,"listens":[]}}`, lbListens)
	got, err := testClient(t, srv.URL).fetch(context.Background(), serviceListenBrainz, "someone")
	if err != nil {
		t.Fatal(err)
	}
	if got.Playing != nil {
		t.Errorf("再生中でないのに値がある: %+v", got.Playing)
	}
	if len(got.Recent) == 0 {
		t.Error("履歴まで落ちている")
	}
}

// mbid の出どころが 3 つあり、どれが入るかは送信側による。
func TestListenBrainz_ReleaseMBIDPriority(t *testing.T) {
	var l lbListen
	l.TrackMetadata.MBIDMapping.CAAReleaseMBID = "caa"
	l.TrackMetadata.AdditionalInfo.ReleaseMBID = "additional"
	l.TrackMetadata.MBIDMapping.ReleaseMBID = "mapping"
	if got := l.releaseMBID(); got != "caa" {
		t.Errorf("Cover Art 用の id を優先していない: %q", got)
	}

	l.TrackMetadata.MBIDMapping.CAAReleaseMBID = ""
	if got := l.releaseMBID(); got != "additional" {
		t.Errorf("送信側の id を使っていない: %q", got)
	}

	l.TrackMetadata.AdditionalInfo.ReleaseMBID = ""
	if got := l.releaseMBID(); got != "mapping" {
		t.Errorf("マッピングの id を使っていない: %q", got)
	}
}

// 履歴が多くても保存が膨らまないこと。
func TestListenBrainz_CapsRecent(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"payload":{"listens":[`)
	for i := range 30 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"listened_at":1,"track_metadata":{"artist_name":"a","track_name":"t"}}`)
	}
	sb.WriteString(`]}}`)

	srv := lbServer(t, `{"payload":{"listens":[]}}`, sb.String())
	got, err := testClient(t, srv.URL).fetch(context.Background(), serviceListenBrainz, "someone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) > maxRecent {
		t.Fatalf("上限を超えて保持している: %d", len(got.Recent))
	}
}

// **利用者が直せるものだけ見せる。** 404 は綴り間違いなので伝える。
func TestListenBrainz_Errors(t *testing.T) {
	for _, tt := range []struct {
		status int
		shown  bool
	}{
		{http.StatusNotFound, true},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
		}))
		_, err := testClient(t, srv.URL).fetch(context.Background(), serviceListenBrainz, "someone")
		srv.Close()
		if err == nil {
			t.Fatalf("status %d でエラーにならない", tt.status)
		}
		ue, ok := err.(*upstreamError)
		if !ok {
			t.Fatalf("status %d: 型が違う (%T)", tt.status, err)
		}
		if (ue.userFacing != "") != tt.shown {
			t.Errorf("status %d: 利用者に見せる=%v", tt.status, ue.userFacing != "")
		}
	}
}

func TestFetch_RejectsBadInput(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")

	// **利用者の文字列が取得先 URL の一部になる。** 経路を変えられる形は通さない。
	for _, name := range []string{"", "a/../b", "a b", "../etc", strings.Repeat("a", 100)} {
		if _, err := c.fetch(context.Background(), serviceListenBrainz, name); err == nil {
			t.Errorf("不正なユーザー名を通した: %q", name)
		}
	}
	if _, err := c.fetch(context.Background(), "spotify", "someone"); err == nil {
		t.Error("対応していないサービスを通した")
	}
}

// 取得元に届かないときはエラーにする (空の結果として保存しない)。
func TestGetJSON_Unreachable(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	if _, err := c.fetch(context.Background(), serviceListenBrainz, "someone"); err == nil {
		t.Fatal("接続失敗を通している")
	}
}

func TestGetJSON_BrokenBody(t *testing.T) {
	srv := lbServer(t, `{not json`, lbListens)
	if _, err := testClient(t, srv.URL).fetch(context.Background(), serviceListenBrainz, "someone"); err == nil {
		t.Fatal("壊れた応答を通している")
	}
}
