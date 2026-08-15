package nowplaying

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shiroha-a/mk/plugin"
	"github.com/shiroha-a/mk/plugin/plugintest"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const testSchema = "plugin_nowplaying_test"

// testDB opens a throwaway schema for one test.
//
// フェイクの DB は使わない。SQL の挙動を模した偽物は本物とずれ、通ったのに
// 本番で落ちる形のテストになる。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`,
		`CREATE SCHEMA ` + testSchema,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

func testConfig(endpoint, lastFmKey string) map[string]any {
	return map[string]any{
		"listenBrainzEndpoint": endpoint,
		"lastFmEndpoint":       endpoint,
		"coverArtEndpoint":     endpoint,
		"lastFmApiKey":         lastFmKey,
		"userAgent":            "test/1.0",
		"timeoutSeconds":       5,
	}
}

func setupRoutes(t *testing.T, endpoint, lastFmKey string) plugintest.Handlers {
	t.Helper()
	return plugintest.New(t).
		WithName("nowplaying").
		WithDB(testDB(t)).
		WithConfig(testConfig(endpoint, lastFmKey)).
		Routes(Plugin)
}

func TestRoutes_SetAndShowProfile(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"u1"}`})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["linked"] != true || m["username"] != "someone" {
		t.Fatalf("想定と違う: %+v", m)
	}
	playing, _ := m["playing"].(*track)
	if playing == nil || playing.Title != "What Else Is There?" {
		t.Fatalf("再生中が出ていない: %+v", m["playing"])
	}
	recent, _ := m["recent"].([]track)
	if len(recent) != 2 {
		t.Errorf("履歴: %+v", m["recent"])
	}
}

// 未登録は「無い」であってエラーではない。
func TestRoutes_ProfileOfUnlinkedUser(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"nobody"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["linked"] != false {
		t.Fatalf("linked=false であるべき: %+v", res)
	}
}

func TestRoutes_ProfileRequiresUserID(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	if _, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{}`}); err == nil {
		t.Fatal("userId 無しを通している")
	}
}

func TestRoutes_RequiresLogin(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	for _, key := range []string{"POST /me", "POST /me/set"} {
		if _, err := h.Call(t, key, plugintest.Request{
			Body: `{"service":"listenbrainz","username":"someone"}`,
		}); err == nil {
			t.Fatalf("%s: 未ログインを弾いていない", key)
		}
	}
}

func TestRoutes_MeWithoutRegistration(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	res, err := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["username"] != nil {
		t.Fatalf("未登録なのに値がある: %+v", m)
	}
	// キーが無ければ Last.fm を選ばせない。
	if m["lastFmAvailable"] != false {
		t.Errorf("API キーが無いのに Last.fm を出している: %+v", m)
	}
}

// **キーが無いインスタンスで Last.fm を選ばせない。** 選ばせてから
// 「使えません」と言うより、理由を添えて断る方がよい。
func TestRoutes_LastFmNeedsAPIKey(t *testing.T) {
	srv := lfServer(t, lfTracks)

	without := setupRoutes(t, srv.URL, "")
	_, err := without.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"lastfm","username":"someone"}`,
	})
	if err == nil {
		t.Fatal("キー無しで Last.fm を通している")
	}
	if !strings.Contains(err.Error(), "API キー") {
		t.Errorf("理由が伝わらない: %v", err)
	}

	with := setupRoutes(t, srv.URL, "test-key")
	if _, err := with.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"lastfm","username":"someone"}`,
	}); err != nil {
		t.Fatalf("キーがあれば通るべき: %v", err)
	}
}

// **存在しないユーザー名を黙って保存しない。**
func TestRoutes_RejectsUnknownUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h := setupRoutes(t, srv.URL, "")

	_, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"nosuchuser"}`,
	})
	if err == nil {
		t.Fatal("エラーにならない")
	}
	if !strings.Contains(err.Error(), "見つかりません") {
		t.Fatalf("理由が伝わらない: %v", err)
	}
}

// 上流の一時的な不調では登録を拒まない。
func TestRoutes_SavesDespiteUpstreamOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	h := setupRoutes(t, srv.URL, "")

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatalf("登録できるべき: %v", err)
	}
	res, _ := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if res.(map[string]any)["username"] != "someone" {
		t.Fatalf("保存されていない: %+v", res)
	}
}

// 上流が落ちていても、登録済みならプロフィールは成立させる。
func TestRoutes_ProfileSurvivesUpstreamOutage(t *testing.T) {
	db := testDB(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()

	h := plugintest.New(t).WithName("nowplaying").WithDB(db).
		WithConfig(testConfig(down.URL, "")).Routes(Plugin)
	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"u1"}`})
	if err != nil {
		t.Fatalf("取得できなくてもエラーにしない: %v", err)
	}
	m := res.(map[string]any)
	if m["linked"] != true {
		t.Errorf("登録自体は残るべき: %+v", m)
	}
	if m["playing"] != nil {
		t.Errorf("取れていないのに値がある: %+v", m["playing"])
	}
}

func TestRoutes_UnlinkWithEmptyUsername(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":""}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, _ := h.Call(t, "POST /me", plugintest.Request{UserID: "u1"})
	if res.(map[string]any)["username"] != nil {
		t.Fatalf("解除されていない: %+v", res)
	}
	// 解除したら手元のデータも消す (残すと次に登録した人の分と混ざる)。
	res, _ = h.Call(t, "POST /profile", plugintest.Request{Body: `{"userId":"u1"}`})
	if res.(map[string]any)["linked"] != false {
		t.Errorf("解除後も残っている: %+v", res)
	}
}

func TestRoutes_RejectsBadInput(t *testing.T) {
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := setupRoutes(t, srv.URL, "")

	for _, body := range []string{
		`{"service":"listenbrainz","username":"a b"}`,
		`{"service":"listenbrainz","username":"../etc"}`,
		`{"service":"spotify","username":"someone"}`,
		`not json`,
	} {
		if _, err := h.Call(t, "POST /me/set", plugintest.Request{UserID: "u1", Body: body}); err == nil {
			t.Errorf("不正な入力を通した: %s", body)
		}
	}
}

// --- ジャケットの中継 ---

func TestRoutes_Art(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/release/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpegdata"))
	}))
	defer srv.Close()
	h := setupRoutes(t, srv.URL, "")

	res, err := h.Call(t, "GET /art/:key", plugintest.Request{
		Params: map[string]string{"key": "mb:" + sampleMBID},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, ok := res.(plugin.Blob)
	if !ok {
		t.Fatalf("Blob で返していない: %T", res)
	}
	if blob.ContentType != "image/jpeg" || len(blob.Body) == 0 {
		t.Errorf("応答: %+v", blob.ContentType)
	}
	if blob.CacheControl == "" {
		t.Error("キャッシュさせていない")
	}

	// **鍵の検証はルートでも効くこと。**
	if _, err := h.Call(t, "GET /art/:key", plugintest.Request{
		Params: map[string]string{"key": "../../etc/passwd"},
	}); err == nil {
		t.Error("不正な鍵を通した")
	}
}

// --- ジョブ ---

// 定期取得はしない (見られる頃には古い)。溜まった行を落とすだけ。
func TestJobs_SweepsOldRows(t *testing.T) {
	db := testDB(t)
	srv := lbServer(t, lbNowPlaying, lbListens)
	routes := plugintest.New(t).WithName("nowplaying").WithDB(db).
		WithConfig(testConfig(srv.URL, "")).Routes(Plugin)
	if _, err := routes.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE snapshots SET fetched_at = now() - interval '60 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO remote_pending (id, host, username, created_at)
		VALUES ('old', 'other.example', 'alice', now() - interval '3 days')
	`); err != nil {
		t.Fatal(err)
	}

	jobs := plugintest.New(t).WithName("nowplaying").WithDB(db).
		WithConfig(testConfig(srv.URL, "")).Jobs(Plugin)
	if err := jobs.Run(t, "sweep", ""); err != nil {
		t.Fatal(err)
	}

	var snaps, pendings int
	if err := db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM remote_pending`).Scan(&pendings); err != nil {
		t.Fatal(err)
	}
	if snaps != 0 {
		t.Errorf("古い行が残っている: %d", snaps)
	}
	if pendings != 0 {
		t.Errorf("答えの返らなかった問い合わせが残っている: %d", pendings)
	}
}

func TestJobs_RegistersSchedule(t *testing.T) {
	jobs := plugintest.New(t).WithName("nowplaying").WithDB(testDB(t)).Jobs(Plugin)

	if len(jobs.Schedules) != 1 || jobs.Schedules[0].Name != "sweep" {
		t.Fatalf("想定と違う: %+v", jobs.Schedules)
	}
}
