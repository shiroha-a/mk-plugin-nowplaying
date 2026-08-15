package nowplaying

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleMBID = "f1418001-7f1e-46af-bfdb-95faeded8841"

// 鍵にできるのは MusicBrainz の id と Last.fm の画像名だけ。
// **URL そのものを持ち回らない** — 中継先を外から決められないようにする。
func TestArtKeys(t *testing.T) {
	if got := artKeyForMBID(sampleMBID); got != "mb:"+sampleMBID {
		t.Errorf("mbid の鍵: %q", got)
	}
	for _, bad := range []string{"", "not-a-uuid", "../etc/passwd", sampleMBID + "x"} {
		if got := artKeyForMBID(bad); got != "" {
			t.Errorf("不正な mbid を通した: %q -> %q", bad, got)
		}
	}

	const img = "https://lastfm.freetls.fastly.net/i/u/300x300/0123456789abcdef0123456789abcdef.png"
	if got := artKeyForLastFm(img); got != "lf:0123456789abcdef0123456789abcdef.png" {
		t.Errorf("Last.fm の鍵: %q", got)
	}
	for _, bad := range []string{
		"",
		"https://evil.example/x.png", // ファイル名の形が違う
		"https://lastfm.freetls.fastly.net/i/u/300x300/x.png", // ハッシュでない
		"https://lastfm.freetls.fastly.net/i/u/300x300/0123456789abcdef0123456789abcdef.svg",
	} {
		if got := artKeyForLastFm(bad); got != "" {
			t.Errorf("不正な画像を通した: %q -> %q", bad, got)
		}
	}

	// 「画像なし」のプレースホルダは中継しない (灰色の四角が出るだけ)。
	placeholder := "https://lastfm.freetls.fastly.net/i/u/300x300/" + lastFmNoImage + ".png"
	if got := artKeyForLastFm(placeholder); got != "" {
		t.Errorf("プレースホルダを通した: %q", got)
	}
}

func TestArtURL(t *testing.T) {
	if got := artURL("mb:" + sampleMBID); got != artRoutePrefix+"mb:"+sampleMBID {
		t.Errorf("proxy URL: %q", got)
	}
	if artURL("") != "" {
		t.Error("空の鍵で URL を作っている")
	}
}

// 鍵から組み立てる URL が、想定した取得先にしか向かないこと。
func TestArtSource(t *testing.T) {
	c := testClient(t, "https://coverart.test")

	got, err := c.artSource("mb:" + sampleMBID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://coverart.test/release/"+sampleMBID+"/"+coverArtSize {
		t.Errorf("Cover Art の URL: %q", got)
	}

	got, err = c.artSource("lf:0123456789abcdef0123456789abcdef.png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://lastfm.freetls.fastly.net/") {
		t.Errorf("Last.fm の URL: %q", got)
	}

	for _, bad := range []string{
		"",
		"http://evil.example/x.png",
		"mb:../../etc",
		"lf:../../etc.png",
		"zz:something",
	} {
		if _, err := c.artSource(bad); err == nil {
			t.Errorf("不正な鍵を通した: %q", bad)
		}
	}
}

// Cover Art Archive は実体を Internet Archive に置いているので、リダイレクトを
// 追わないと取れない。**追う先は必ず検査する。**
func TestAllowedArtHost(t *testing.T) {
	// **ノードの形は 1 つではない。** ia<数字>.us だけを許していたら
	// dn<数字>.ca の系統で弾かれてジャケットが出なかった (実運用で踏んだ)。
	ok := []string{
		"coverartarchive.org", "archive.org",
		"ia801404.us.archive.org",
		"dn710006.ca.archive.org",
		"lastfm.freetls.fastly.net",
	}
	for _, h := range ok {
		if !allowedArtHost(h) {
			t.Errorf("許すべきホストを弾いた: %q", h)
		}
	}

	ng := []string{
		"evil.example",
		// **末尾一致では通してはいけない形。**
		"archive.org.evil.example",
		"ia1.us.archive.org.evil.com",
		"notarchive.org",
		"localhost",
		"127.0.0.1",
	}
	for _, h := range ng {
		if allowedArtHost(h) {
			t.Errorf("弾くべきホストを通した: %q", h)
		}
	}
}

func TestArt_FetchesAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/release/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpegdata"))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	ctx := context.Background()

	got, err := c.art(ctx, "mb:"+sampleMBID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.data) != "jpegdata" || got.contentType != "image/jpeg" {
		t.Errorf("応答: %+v", got)
	}

	// 2 回目は取りに行かない。ジャケットは変わらない。
	if _, err := c.art(ctx, "mb:"+sampleMBID); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("取得回数: %d (キャッシュが効いていない)", hits)
	}

	if _, err := c.art(ctx, "bogus"); err == nil {
		t.Error("不正な鍵を取りに行っている")
	}
}

// **取得元が名乗る型をそのまま返さない。**
func TestArt_RejectsNonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	}))
	defer srv.Close()

	if _, err := testClient(t, srv.URL).art(context.Background(), "mb:"+sampleMBID); err == nil {
		t.Error("画像でない応答を通している")
	}
}

func TestNormalizeImageType(t *testing.T) {
	cases := map[string]string{
		"image/png":                "image/png",
		"image/gif":                "image/gif",
		"image/webp":               "image/webp",
		"image/jpeg":               "image/jpeg",
		"image/jpeg; charset=utf8": "image/jpeg",
		// 判別できないものは JPEG に寄せる (画像であることは確認済み)。
		"image/tiff": "image/jpeg",
	}
	for in, want := range cases {
		if got := normalizeImageType(in); got != want {
			t.Errorf("%q -> %q (期待 %q)", in, got, want)
		}
	}
}

// 許していないホストへ飛ばされたら追わないこと。
func TestArtHTTPClient_BlocksForeignRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("stolen"))
	}))
	defer evil.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/x.jpg", http.StatusFound)
	}))
	defer src.Close()

	if _, err := testClient(t, src.URL).art(context.Background(), "mb:"+sampleMBID); err == nil {
		t.Fatal("許していないホストへのリダイレクトを追っている")
	}
}

func TestArtCache_Evicts(t *testing.T) {
	c := newArtCache(10)
	c.put("a", artBlob{data: []byte("12345")})
	c.put("b", artBlob{data: []byte("12345")})
	if _, ok := c.get("a"); !ok {
		t.Fatal("入れたものが取れない")
	}

	c.put("c", artBlob{data: []byte("12345")})
	if _, ok := c.get("a"); ok {
		t.Error("古いものが残っている")
	}

	// 上限より大きいものは持たない (1 つで全部追い出してしまう)。
	c.put("huge", artBlob{data: make([]byte, 99)})
	if _, ok := c.get("huge"); ok {
		t.Error("上限を超えるものを抱えている")
	}

	var nilCache *artCache
	nilCache.put("x", artBlob{data: []byte("y")})
	if _, ok := nilCache.get("x"); ok {
		t.Error("nil キャッシュが値を返している")
	}
}

// 相手のインスタンスが返した URL を、こちらのプロキシに貼り替えること。
func TestRewriteArtURL(t *testing.T) {
	remote := "https://other.example" + artRoutePrefix + "mb:" + sampleMBID
	if got := rewriteArtURL(remote); got != artRoutePrefix+"mb:"+sampleMBID {
		t.Errorf("貼り替えられていない: %q", got)
	}

	// 素の URL は触らない。
	if got := rewriteArtURL("https://example.test/x.png"); got != "https://example.test/x.png" {
		t.Errorf("関係ない URL を書き換えた: %q", got)
	}

	// 想定外の鍵は落とす。相手が渡した文字列をそのまま取得先にしない。
	for _, bad := range []string{
		"https://other.example" + artRoutePrefix + "../../etc",
		"https://other.example" + artRoutePrefix,
		"https://other.example" + artRoutePrefix + "zz:x",
	} {
		if got := rewriteArtURL(bad); got != "" {
			t.Errorf("不正な鍵を通した: %q -> %q", bad, got)
		}
	}
}

// 同じ鍵を二重に入れない (入れ替えると使用量の勘定が狂う)。
func TestArtCache_IgnoresDuplicate(t *testing.T) {
	c := newArtCache(100)
	c.put("a", artBlob{data: []byte("12345")})
	c.put("a", artBlob{data: []byte("6789012345")})

	got, ok := c.get("a")
	if !ok || string(got.data) != "12345" {
		t.Errorf("後から入れた方で上書きされている: %q", got.data)
	}
	if c.bytes != 5 {
		t.Errorf("使用量が二重に計上されている: %d", c.bytes)
	}
}
