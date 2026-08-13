# Goで作る自作データベース — SQLiteに学ぶデータベース内部構造

SQLite を参考にしながら Go でデータベース(minidb)を自作して学ぶ本の
リポジトリです。**Markdown 原稿(`docs/`)を唯一の正**とし、
そこから Web サイトを生成します。

```
docs/   ──> Docusaurus ──> Web サイト(GitHub Pages)
```

## 構成

```
.
├── docs/                  # 原稿(Markdown・これが正)
│   └── index.md           # まえがき(第1部以降は今後の PR で追加)
├── code/                  # 章ごとスナップショット方式
│   ├── ch02/               # 第2章末時点の完成コード(独立 Go モジュール)
│   ├── ch03/               # 第3章末時点の完成コード(独立 Go モジュール)
│   └── ...                 # (今後の PR で追加)
├── src/ / static/ ...     # Docusaurus(サイト側)
└── .github/workflows/     # CI: フォーマット検証 / ビルド検証 / Pages デプロイ
```

## 使い方

```sh
bun install             # 初回のみ
make dev                # 執筆用ローカルサーバー (http://localhost:3000)
make web                # サイトのビルド(build/)
make test               # 掲載コードの検証(go vet + go test)
```

## 公開手順(GitHub Pages)

1. リポジトリの Settings → Pages → Source を「GitHub Actions」に設定
2. Pages 設定にカスタムドメイン `godb.hirano00o.dev` を登録し、DNS で CNAME を `hirano00o.github.io` に向ける
3. main に push すると、ビルド → サイトデプロイが自動実行される

## 原稿の書き方(第2部以降)

- 章ファイルを `docs/part2/ch05.md` のように追加し、`sidebars.ts` に 1 行ずつ追記する
- コード掲載は「`**キャプション**` の直後に ` ```go ` フェンス」の形式で書く
- 端末表示は ` ```console ` フェンス(`$ ` 始まりの行はプロンプト色になる)
- 図・囲み(`chap-goal` / `column-note` / `sqlite-note` / `warn` / `diagram`)は
  生 HTML で書く。`src/css/book.css` が装飾する
- `.md` は CommonMark として解釈される設定(`markdown.format: 'detect'`)。
  React コンポーネントを使う対話ページだけ `.mdx` にする

## 検証環境

- Go 1.26.5 / SQLite 3.53.3(公式ミラー sqlite/sqlite のソースからビルド)
- 本文中のコード・実行結果・16進ダンプはすべて実機採取
