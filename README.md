# Goで作る自作データベース — SQLiteに学ぶデータベース内部構造

SQLite を参考にしながら Go でデータベース(minidb)を自作して学ぶ本の
リポジトリです。**Markdown 原稿(`docs/`)を唯一の正**とし、
そこから Web サイトを生成します。

```
docs/   ──> Docusaurus ──> Web サイト(https://godb.hirano00o.dev)
```

## 構成

```
.
├── docs/                  # 原稿(Markdown・これが正)
│   ├── index.md           # まえがき
│   ├── part1/             # 第1部(完結)
│   ├── part2/             # 第2部(完結)
│   ├── part3/             # 第3部(完結)
│   └── part4/             # 第4部(完結)
├── code/                  # 章ごとスナップショット方式(コードに変更のあった章のみ)
│   ├── ch02/               # 第2章末時点の完成コード(独立 Go モジュール)
│   ├── ch03/               # 第3章末時点の完成コード(独立 Go モジュール)
│   └── ... ch16/           # 以降、最終章の第16章まで各章の完成コード
├── src/ / static/ ...     # Docusaurus(サイト側)
├── Dockerfile, deploy/    # 配信用イメージ(nginx)
└── .github/workflows/     # CI: フォーマット検証 / ビルド検証、Release: イメージ公開
```

## 使い方

```sh
bun install             # 初回のみ
make dev                # 執筆用ローカルサーバー (http://localhost:3000)
make web                # サイトのビルド(build/)
make test               # 掲載コードの検証(go vet + go test)
```

## 検証環境

- Go 1.26.5 / SQLite 3.53.3(公式ミラー sqlite/sqlite のソースからビルド)
- 本文中のコード・実行結果・16進ダンプはすべて実機採取
