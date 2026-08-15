# mk-plugin-nowplaying

[mk-go](https://github.com/shiroha-a/mk) のサーバープラグイン。利用者が聴いている曲をプロフィールに表示する。

取得元は [ListenBrainz](https://listenbrainz.org/) と [Last.fm](https://www.last.fm/)。

```
プロフィール
┌────────────────────────────────────────────┐
│ ▣ 再生中  What Else Is There?  Röyksopp   ▾ │ ← 既定はこの 1 行だけ
└────────────────────────────────────────────┘
   ↓ クリックで開く
┌────────────────────────────────────────────┐
│ ▣ Roygbiv          Boards of Canada    3分前 │
│ ▣ Xtal             Aphex Twin         28分前 │
│ ▣ Everything In Its Right Place  Radiohead  │
│ ListenBrainz · someone                      │
└────────────────────────────────────────────┘
```

再生中でなければ、直近に聴いた曲を「最近」として出す。どちらも無ければ行ごと出さない。

## 導入

mk-go は**プラグインをビルド時に組み込む**。`plugins/` に置いてビルドし直す。

```bash
# mk-go のリポジトリルートで
cd plugins
git clone https://github.com/shiroha-a/mk-plugin-nowplaying nowplaying
cd ..

make build                # plugins/ を走査して取り込む
make uds-frontend-build   # フロントエンドも作り直す
make uds-up               # 再起動しないと画面が変わらない
```

> **注意**: 配布されている Docker image を pull するだけでは使えない。ビルド時組み込みなので、プラグインを使うインスタンスは自分でビルドする必要がある。

## 使い方

利用者が **設定 → プロフィール** でサービスとユーザー名を入れる。

登録時に一度取得して存在を確かめるので、綴りを間違えていればその場で分かる。

### ListenBrainz と Last.fm の違い

| | ListenBrainz | Last.fm |
|---|---|---|
| インスタンス側の準備 | **不要** | API キーが要る |
| 利用者の準備 | ユーザー名だけ | ユーザー名だけ |

**ListenBrainz は認証不要**なので、何も設定しなくても使える。Last.fm を使いたい場合は、[API アカウント](https://www.last.fm/api/account/create)を作ってキーを設定に入れる。キーが無いインスタンスでは、そもそも Last.fm を選択肢に出さない。

## 設定

省略できる。既定で ListenBrainz が使える。

```yaml
# .config/default.yml
plugins:
  nowplaying:
    enabled: true
    lastFmApiKey: ""                              # 入れると Last.fm も選べる
    listenBrainzEndpoint: https://api.listenbrainz.org
    lastFmEndpoint: https://ws.audioscrobbler.com
    coverArtEndpoint: https://coverartarchive.org
    userAgent: mk-go-plugin-nowplaying/0.1 (+https://github.com/shiroha-a/mk)
    timeoutSeconds: 10
```

## リモート利用者

**同じプラグインを入れた mk-go 同士**なら、他インスタンスの利用者のプロフィールにも出る。経路は mk-go 専用の peer channel (ActivityPub には出ない、mk-go #2537)。

- 相手が同じプラグインを持たなければ何も出ない
- **問い合わせは非同期**なので、初めて開いたときは出ない
- 取り寄せた分は 2 分覚える
- 相手に送るのは **username だけ**
- 相手が返すジャケット URL は、こちらのプロキシ経由に貼り替える

## 取得元への配慮

- **開かれたときに取る。定期取得はしない。** 再生中の曲は数分で変わるので、先回りしても見られる頃には古い。全員分を定期的に取ると、聴いていない人の分まで無駄に叩くことになる
- **30 秒キャッシュ**を挟む。プロフィールを連打されても外向きのリクエストは 30 秒に 1 回まで
- **取り直しは裏で行う。** 手元のものを即返してから更新するので、取得元が遅い日でもプロフィールの表示は待たされない
- **`User-Agent` を名乗る。** MusicBrainz 系は明示的に求めている

## ジャケット画像

Cover Art Archive (ListenBrainz 側) と Last.fm の CDN から取り、**同一オリジンで中継**する (`/api/plugin/nowplaying/art/<key>`)。mk-go の CSP が `img-src 'self'` なので、緩めずに表示するため。

**任意の URL は中継しない。** ここは外部から与えられた文字列で外向きのリクエストを出す場所なので、そのまま通すと SSRF の入口になる。受け取るのは

- MusicBrainz の release id (UUID)
- Last.fm の画像ハッシュ (32 桁の hex)

だけで、**URL はこちらで組み立てる**。Cover Art Archive は実体を Internet Archive に置いているためリダイレクトを追わざるを得ないが、追う先のホストを毎ホップ検査している。

## 開発

```bash
cd plugins/nowplaying && go test ./...
```

DB は実 PostgreSQL を使う (`TEST_DB_*` 環境変数)。取得元は `httptest` で差し替えるので、外部には出ない。

### 踏みやすい罠

- **Last.fm の `track` は 1 件のときオブジェクトになる** — 配列で決め打つと、聴取が 1 件しかない利用者で丸ごと壊れる。JSON なのに XML の名残がある形 (`#text` に本体が入る、`@attr` に属性が入る) も同様
- **Last.fm は 200 でエラーを返す** — ステータスだけ見ていると、存在しない利用者を「聴取なし」として保存してしまう。`error` フィールドを必ず見る
- **再生中に再生時刻は無い** — 付いているように扱うと「たった今」が出続ける
- **ListenBrainz の mbid は 3 か所に分かれる** — 送信側がタグを付けていなければ `additional_info` は空で、`mbid_mapping` 側にしか無い。Cover Art 用に選ばれた `caa_release_mbid` が最も確実
- **Last.fm には「画像なし」のプレースホルダがある** — 中継すると灰色の四角が出るだけなので、ハッシュで弾く

## ライセンス

AGPL-3.0-only。mk-go 本体と同じ。
