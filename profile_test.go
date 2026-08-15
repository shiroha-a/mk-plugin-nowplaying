package nowplaying

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiroha-a/mk/plugin/plugintest"
)

// 同じ利用者の取り直しが何本も走らないこと。プロフィールを連打されたときに
// 外向きのリクエストが積み上がる。
func TestRefreshGuard(t *testing.T) {
	g := newRefreshGuard()

	if !g.enter("u1") {
		t.Fatal("1 本目が弾かれた")
	}
	if g.enter("u1") {
		t.Error("同じ利用者で 2 本目が走ろうとしている")
	}
	// 別の利用者は独立して走ってよい。
	if !g.enter("u2") {
		t.Error("別の利用者まで止めている")
	}

	g.leave("u1")
	if !g.enter("u1") {
		t.Error("終わった後に再開できない")
	}
}

func TestProfilePayload_EmptyRecent(t *testing.T) {
	// recent が nil のまま JSON にすると null になり、frontend が
	// `.length` で落ちる。空配列に正規化する。
	got := profilePayload("listenbrainz", "someone", &snapshot{}, time.Now())
	recent, ok := got["recent"].([]track)
	if !ok || recent == nil {
		t.Fatalf("recent が空配列でない: %#v", got["recent"])
	}
	if got["playing"] != (*track)(nil) {
		t.Errorf("再生中が nil でない: %#v", got["playing"])
	}
}

// 期限が切れていたら**裏で**取り直すこと。呼び出し元は待たされない。
func TestBuildProfile_RefreshesInBackground(t *testing.T) {
	db := testDB(t)

	// 1 回目と 2 回目で違う曲を返す。裏の取り直しが効いたかを見分けるため。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			// 登録時の 2 回 (playing-now と listens)。
			serveLB(w, r, lbNowPlaying, lbListens)
			return
		}
		serveLB(w, r,
			`{"payload":{"listens":[{"track_metadata":{"artist_name":"Aphex Twin","track_name":"Xtal"}}]}}`,
			`{"payload":{"listens":[]}}`)
	}))
	defer srv.Close()

	h := plugintest.New(t).WithName("nowplaying").WithDB(db).
		WithConfig(testConfig(srv.URL, "")).Routes(Plugin)
	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}

	// 期限切れにしてから開く。**この呼び出し自体は古いものを返す。**
	if _, err := db.Exec(`UPDATE snapshots SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"u1"}`})
	if err != nil {
		t.Fatal(err)
	}
	playing, _ := res.(map[string]any)["playing"].(*track)
	if playing == nil || playing.Title != "What Else Is There?" {
		t.Fatalf("古いものを返していない: %+v", res)
	}

	// 裏の取り直しが DB に反映されるのを待つ。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var title string
		err := db.QueryRow(`SELECT playing->>'title' FROM snapshots WHERE user_id = 'u1'`).Scan(&title)
		if err == nil && title == "Xtal" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("裏で取り直されていない")
}

// serveLB routes one request to the right canned body.
func serveLB(w http.ResponseWriter, r *http.Request, now, listens string) {
	if len(r.URL.Path) > 0 && r.URL.Path[len(r.URL.Path)-1:] == "w" {
		// .../playing-now
		_, _ = w.Write([]byte(now))
		return
	}
	_, _ = w.Write([]byte(listens))
}

// リダイレクトの検査そのものを直接叩く。実際の Internet Archive まで行かずに
// 分岐を確かめられる。
func TestArtHTTPClient_CheckRedirect(t *testing.T) {
	c := newArtHTTPClient(time.Second)

	ok := httptest.NewRequest(http.MethodGet, "https://ia801404.us.archive.org/x.jpg", nil)
	if err := c.CheckRedirect(ok, nil); err != nil {
		t.Errorf("許すべきリダイレクトを弾いた: %v", err)
	}

	// https から降りない。平文に落とされると経路上で差し替えられる。
	plain := httptest.NewRequest(http.MethodGet, "http://ia801404.us.archive.org/x.jpg", nil)
	if err := c.CheckRedirect(plain, nil); err == nil {
		t.Error("http へのリダイレクトを許している")
	}

	evil := httptest.NewRequest(http.MethodGet, "https://evil.example/x.jpg", nil)
	if err := c.CheckRedirect(evil, nil); err == nil {
		t.Error("許していないホストへのリダイレクトを許している")
	}

	// 転送が続きすぎたら諦める。
	var via []*http.Request
	for range 5 {
		via = append(via, ok)
	}
	if err := c.CheckRedirect(ok, via); err == nil {
		t.Error("リダイレクトの上限が効いていない")
	}
}
