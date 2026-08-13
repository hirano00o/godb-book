# ターゲット一覧: dev(執筆用ローカルサーバー) / web(サイトビルド) / test(掲載コードの検証) / fmt(整形)
.PHONY: dev web test fmt

dev:            ## 執筆用ローカルサーバー
	bun run start

web:            ## Docusaurus サイトをビルド
	bun run build

test:           ## 掲載コードの検証(go vet + go test、code/ 未追加でも正常終了)
	for mod in code/ch*/go.mod; do \
		[ -f "$$mod" ] || continue; \
		dir=$$(dirname "$$mod"); \
		(cd "$$dir" && go vet ./... && go test ./...) || exit 1; \
	done

fmt:            ## コードの整形
	bun run fmt
