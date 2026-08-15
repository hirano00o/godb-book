// cmd/seeddemo/main.go
// users テーブルへダミー行を大量投入するベンチマーク用のツール。
//
// btree を直接操作するのではなく exec.Engine.Execute のループで
// INSERT を発行する。これは「アプリケーションから見た minidb」の
// 素直な使用例を兼ねる(内部実装ではなく SQL だけでデータを増やせる)。
//
// 使い方:
//
//	go run ./cmd/seeddemo <db ファイル> <件数 N>
//
// users テーブル(id INTEGER PRIMARY KEY, name TEXT, age INTEGER)が
// 無ければ作成し、既存の最大 id の続きから N 件を
// INSERT INTO users VALUES (i, 'user<i>', i % 100) の形で挿入する。
package main

import (
	"fmt"
	"os"
	"strconv"

	"minidb/exec"
	"minidb/pager"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "使い方: seeddemo <db ファイル> <件数 N>")
		os.Exit(1)
	}
	n, err := strconv.Atoi(os.Args[2])
	if err != nil || n < 0 {
		fmt.Fprintln(os.Stderr, "件数 N は 0 以上の整数で指定してください")
		os.Exit(1)
	}

	pg, err := pager.Open(os.Args[1])
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

	startID, err := ensureUsersTable(engine)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for i := 0; i < n; i++ {
		id := startID + i
		stmt := fmt.Sprintf("INSERT INTO users VALUES (%d, 'user%d', %d)", id, id, id%100)
		if _, err := engine.Execute(stmt); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	fmt.Printf("users に %d 件を挿入しました(合計 %d 件)\n", n, startID-1+n)
}

// ensureUsersTable は users テーブルが無ければ作成する。
// 戻り値は次に使う id(既存の最大 id + 1、テーブルが空/未作成なら 1)。
func ensureUsersTable(engine *exec.Engine) (int, error) {
	tables, err := engine.Tables()
	if err != nil {
		return 0, err
	}
	exists := false
	for _, t := range tables {
		if t == "users" {
			exists = true
			break
		}
	}
	if !exists {
		if _, err := engine.Execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)"); err != nil {
			return 0, err
		}
		return 1, nil
	}

	res, err := engine.Execute("SELECT id FROM users")
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, row := range res.Rows {
		if id := int(row[0].Int); id > maxID {
			maxID = id
		}
	}
	return maxID + 1, nil
}
