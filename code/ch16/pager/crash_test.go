package pager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// crashSentinel は障害注入テスト専用の panic 値。os.Exit と違って
// テストプロセス自体を道連れにしないよう、注入点に到達したら
// crashPoint の呼び出し元(pager 内部)から一気に巻き戻り、テストが
// recover して「クラッシュが起きた」ことを検知する。
type crashSentinel struct{ point string }

// simulateCrashAt は debugCrashHook を point 到達時に crashSentinel を
// panic するよう設定する。以後のコミット処理は途中で巻き戻り、Flush や
// Commit の後始末(ジャーナル削除・fsync 等)は一切実行されない。
func simulateCrashAt(point string) {
	SetDebugCrashHook(func(p string) {
		if p == point {
			panic(crashSentinel{point: p})
		}
	})
}

// crashClose はクラッシュ相当の後始末を行うテスト用ヘルパー。Flush /
// Commit / チェックポイントを一切通さず、DB ファイルの fd だけを生で
// close する。flock は fd に紐付くので、これだけで解放される
// (カーネルがクラッシュしたプロセスの fd を後始末するのと同じ)。
// 放置した Pager をそのまま残すと、同一プロセス内では fd が生きたままの
// flock が新しい Open を ErrBusy にしてしまうため、この後始末が必要になる。
func crashClose(t *testing.T, p *Pager) {
	t.Helper()
	if err := p.file.Close(); err != nil {
		t.Fatal(err)
	}
}

// runUntilCrash は fn(通常はトランザクション 1 つ分の Begin→変更→Commit)
// を実行し、simulateCrashAt で仕込んだ注入点に到達して panic することを
// 期待する。到達しなかった(= 正常終了してしまった)場合はテストを失敗させる。
func runUntilCrash(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("クラッシュフックが発火せず、正常終了してしまいました")
		}
		if _, ok := r.(crashSentinel); !ok {
			panic(r) // 想定外の panic はそのまま伝播させる
		}
	}()
	fn()
}

// TestCrashAfterJournalRecovers は、ロールバックジャーナルモードの
// コミットが "after-journal"(ジャーナル書き込み完了・本体反映前)で
// クラッシュした場合、次回 Open の recoverFromJournal が未完の
// トランザクションを正しく巻き戻すことを確認する。
func TestCrashAfterJournalRecovers(t *testing.T) {
	t.Cleanup(func() { SetDebugCrashHook(nil) })

	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// 整備コミット: 1 行目相当の変更を普通にコミットする。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	row1, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(row1.Data[:], "row1")
	p.MarkDirty(row1.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	numPagesAfterRow1 := p.NumPages()

	simulateCrashAt("after-journal")

	runUntilCrash(t, func() {
		if err := p.Begin(); err != nil {
			t.Fatal(err)
		}
		row2, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		copy(row2.Data[:], "row2")
		p.MarkDirty(row2.ID)
		_ = p.Commit() // ここで crashSentinel が panic するはず
	})

	crashClose(t, p)

	if _, err := os.Stat(path + "-journal"); err != nil {
		t.Fatalf("クラッシュ直後はジャーナルが残っているはず: err=%v", err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatalf("クラッシュ後の再オープンが失敗しました: %v", err)
	}
	defer p2.Close()

	if p2.NumPages() != numPagesAfterRow1 {
		t.Fatalf("再オープン後の NumPages = %d, want %d(2 行目の Allocate が巻き戻っていない)", p2.NumPages(), numPagesAfterRow1)
	}

	got1, err := p2.Get(row1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got1.Data[:], []byte("row1")) {
		t.Fatalf("1 行目の内容 = %q, want prefix %q", got1.Data[:4], "row1")
	}

	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Fatalf("再オープン後もジャーナルファイルが残っています: err=%v", err)
	}
}

// TestCrashMidWALRecovers は、WAL モードのコミットが "mid-wal-commit"
// (フレーム書き込み完了・fsync 前)でクラッシュした場合、次回 Open の
// openWALIfPresent が尻切れフレームを切り詰め、コミット済みの内容だけが
// 見えることを確認する。
func TestCrashMidWALRecovers(t *testing.T) {
	t.Cleanup(func() { SetDebugCrashHook(nil) })

	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	// 整備コミット: 1 行目相当の変更を普通にコミットする。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	row1, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(row1.Data[:], "row1")
	p.MarkDirty(row1.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	walPath := path + "-wal"
	committedInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	committedSize := committedInfo.Size()

	simulateCrashAt("mid-wal-commit")

	runUntilCrash(t, func() {
		if err := p.Begin(); err != nil {
			t.Fatal(err)
		}
		row2, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		copy(row2.Data[:], "row2")
		p.MarkDirty(row2.ID)
		_ = p.Commit() // ここで crashSentinel が panic するはず
	})

	tornInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if tornInfo.Size() <= committedSize {
		t.Fatalf("クラッシュ直後の WAL サイズ = %d, want > %d(尻切れフレームが書かれているはず)", tornInfo.Size(), committedSize)
	}

	crashClose(t, p)

	p2, err := Open(path)
	if err != nil {
		t.Fatalf("クラッシュ後の再オープンが失敗しました: %v", err)
	}
	defer p2.Close()

	got1, err := p2.Get(row1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got1.Data[:], []byte("row1")) {
		t.Fatalf("1 行目の内容 = %q, want prefix %q", got1.Data[:4], "row1")
	}

	walAfter, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if walAfter.Size() != committedSize {
		t.Fatalf("再オープン後の WAL サイズ = %d, want %d(尻切れが切り詰められていない)", walAfter.Size(), committedSize)
	}
}
