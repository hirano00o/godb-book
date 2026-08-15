// cmd/crashdemo/main.go
// クラッシュ実験用のデモツール。コミットプロトコルの特定の瞬間に
// os.Exit でプロセスを即死させ、次に minidb で開き直したときの復旧の
// 様子を観察するために使う。
//
// 使い方:
//
//	go run ./cmd/crashdemo <db ファイル> <シナリオ>
//
// シナリオ:
//
//	uncommitted  — BEGIN して 1 行 INSERT した状態で即死(コミットしない)
//	journal      — コミット中、ジャーナル書き込み完了・本体反映前に即死
//	wal          — (WAL モードの DB で)コミット中、WAL フレーム書き込み後・fsync 前に即死
package main

import (
	"fmt"
	"os"

	"minidb/exec"
	"minidb/pager"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "使い方: crashdemo <db ファイル> <シナリオ>")
		os.Exit(1)
	}
	path, scenario := os.Args[1], os.Args[2]

	pg, err := pager.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pg.Close()

	engine, err := exec.NewEngine(pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// テーブル作成と既存行の確認だけの整備コミットは、以降のシナリオの
	// 準備を単純にするため、シナリオに関係なく普通に成功させる。
	if err := ensureUsersTable(engine); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch scenario {
	case "uncommitted":
		runUncommitted(engine)
	case "journal":
		runJournal(engine)
	case "wal":
		runWAL(engine)
	default:
		fmt.Fprintf(os.Stderr, "不明なシナリオです: %s(uncommitted / journal / wal のいずれかを指定してください)\n", scenario)
		os.Exit(1)
	}
}

// ensureUsersTable は users テーブルが無ければ作成する。
func ensureUsersTable(engine *exec.Engine) error {
	tables, err := engine.Tables()
	if err != nil {
		return err
	}
	for _, t := range tables {
		if t == "users" {
			return nil
		}
	}
	_, err = engine.Execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	return err
}

// runUncommitted は BEGIN して 1 行 INSERT した状態で、COMMIT せずに
// 即死する。トランザクション中はディスクへ一切書かないため、次回 Open
// すればこの行は影も形もない。
func runUncommitted(engine *exec.Engine) {
	if _, err := engine.Execute("BEGIN"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := engine.Execute("INSERT INTO users VALUES (900, 'Doomed', 0)"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("コミットせずに即死します")
	os.Exit(1)
}

// runJournal は autocommit の INSERT が、ジャーナル書き込み完了・本体
// 反映前(pager.Commit の "after-journal" 注入点)に即死するよう仕込む。
func runJournal(engine *exec.Engine) {
	pager.SetDebugCrashHook(func(point string) {
		if point != "after-journal" {
			return
		}
		fmt.Println("ジャーナル書き込み直後・本体反映前に即死します")
		os.Exit(1)
	})
	if _, err := engine.Execute("INSERT INTO users VALUES (901, 'Phantom', 0)"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// フックが発火して途中で死ぬはずなので、ここには到達しない。
	fmt.Fprintln(os.Stderr, "想定外: クラッシュフックが発火せず、コミットが正常終了しました")
	os.Exit(1)
}

// runWAL は autocommit の INSERT が、WAL フレーム書き込み後・fsync 前
// (pager.walCommit の "mid-wal-commit" 注入点)に即死するよう仕込む。
// 呼び出し側が事前に PRAGMA journal_mode = WAL で WAL モードに切り替えて
// おく必要がある。
func runWAL(engine *exec.Engine) {
	pager.SetDebugCrashHook(func(point string) {
		if point != "mid-wal-commit" {
			return
		}
		fmt.Println("WAL フレーム書き込み後・fsync 前に即死します")
		os.Exit(1)
	})
	if _, err := engine.Execute("INSERT INTO users VALUES (902, 'Torn', 0)"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "想定外: クラッシュフックが発火せず、コミットが正常終了しました(journal_mode は WAL になっていますか?)")
	os.Exit(1)
}
