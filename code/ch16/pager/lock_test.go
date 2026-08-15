package pager

import (
	"errors"
	"path/filepath"
	"testing"
)

// flock はプロセス単位ではなく fd(オープンファイル記述)単位のロックなので、
// 同一プロセス内で 2 つの Pager を同じパスに対して開くだけで、別々の接続
// 同士のロック競合を再現できる(2 プロセスを起動する必要がない)。
// このファイルのテストはすべてこの性質を利用する。

// 2 つの接続が同時に読み取り専用トランザクションを開始できる
// (共有ロック同士は競合しない)ことを確認する。
func TestTwoReadersCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.Begin(); err != nil {
		t.Fatalf("A の Begin が失敗しました: %v", err)
	}
	if err := b.Begin(); err != nil {
		t.Fatalf("B の Begin が失敗しました(共有ロック同士のはずが競合しました): %v", err)
	}

	if _, err := a.Get(headerPageID); err != nil {
		t.Fatalf("A の Get が失敗しました: %v", err)
	}
	if _, err := b.Get(headerPageID); err != nil {
		t.Fatalf("B の Get が失敗しました: %v", err)
	}

	if err := a.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// A が読み取りトランザクション(共有ロック)を保持している間、B が書き込みを
// Commit しようとすると、共有 → 排他への昇格に失敗して ErrBusy になる。
// このとき B のトランザクションは生きたままであることを確認し、A が
// Rollback してロックを手放した後は B の Commit 再試行が成功することを
// 確かめる。
func TestCommitBusyWhileReaderActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.Begin(); err != nil {
		t.Fatalf("A の Begin が失敗しました: %v", err)
	}

	if err := b.Begin(); err != nil {
		t.Fatalf("B の Begin が失敗しました: %v", err)
	}
	page, err := b.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "from B")
	b.MarkDirty(page.ID)

	if err := b.Commit(); !errors.Is(err, ErrBusy) {
		t.Fatalf("B の Commit() = %v, want ErrBusy(A が共有ロックを保持中)", err)
	}
	if !b.InTx() {
		t.Fatal("Commit が ErrBusy で失敗した後も B のトランザクションは生きているはず")
	}

	if err := a.Rollback(); err != nil {
		t.Fatalf("A の Rollback が失敗しました: %v", err)
	}

	if err := b.Commit(); err != nil {
		t.Fatalf("A が手放した後の B の Commit 再試行が失敗しました: %v", err)
	}
	if b.InTx() {
		t.Fatal("Commit 成功後も InTx() が true のままです")
	}
}

// TestBeginBusyWhileCommitting(A の Commit 中に B の Begin が ErrBusy になる
// ケース)は、共有 → 排他への昇格からコミット完了までの競合窓が短く
// (writeJournal からジャーナル削除までの間だけ)、通常の I/O 速度では
// 安定して再現できないため省略する。

// A・B が同じ DB をロールバックジャーナルモードで開いている状態で、
// 一方がコミットした変更が、もう一方の次の Begin で(ディスク上の
// 変更カウンタとの差分検知により)キャッシュ無効化を経て見えることを
// 確認する。往復(A → B、B → A)の両方向を検証する。
func TestStaleCacheInvalidatedByChangeCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// A → B: A が書いた内容を B が見える。
	if err := a.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "from A")
	a.MarkDirty(page.ID)
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := b.Begin(); err != nil {
		t.Fatalf("B の Begin が失敗しました: %v", err)
	}
	got, err := b.Get(page.ID)
	if err != nil {
		t.Fatalf("B の Get が失敗しました(キャッシュ無効化後に numPages が更新されていない?): %v", err)
	}
	if string(got.Data[:6]) != "from A" {
		t.Fatalf("B から見えた内容 = %q, want %q(A のコミットが見えていない = 陳腐化したキャッシュ)", got.Data[:6], "from A")
	}
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}

	// B → A: 逆方向も同様に見える。
	if err := b.Begin(); err != nil {
		t.Fatal(err)
	}
	page2, err := b.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page2.Data[:], "from B")
	b.MarkDirty(page2.ID)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := a.Begin(); err != nil {
		t.Fatalf("A の Begin が失敗しました: %v", err)
	}
	got2, err := a.Get(page2.ID)
	if err != nil {
		t.Fatalf("A の Get が失敗しました: %v", err)
	}
	if string(got2.Data[:6]) != "from B" {
		t.Fatalf("A から見えた内容 = %q, want %q(B のコミットが見えていない)", got2.Data[:6], "from B")
	}
	if err := a.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// WAL モードは接続単位で排他ロックを保持するため単一接続専用になる。
// A が Open している間は B の Open が ErrBusy になり、A が Close して
// ロックを手放すと B が Open できるようになることを確認する。
func TestWALModeSingleConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("B の Open() = %v, want ErrBusy(A が WAL の排他ロックを保持中)", err)
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := Open(path)
	if err != nil {
		t.Fatalf("A が Close した後の B の Open が失敗しました: %v", err)
	}
	defer b.Close()
	if b.JournalMode() != JournalModeWAL {
		t.Fatalf("B が開いたモード = %v, want %v", b.JournalMode(), JournalModeWAL)
	}
}

// コミットのたびにディスク上の変更カウンタが 1 ずつ増え、変更のない
// (dirty ページが 1 枚もない)トランザクションでは増えないことを確認する。
func TestChangeCounterIncrements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	initial, err := p.changeCounterOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	if initial != 0 {
		t.Fatalf("新規ファイルの変更カウンタ = %d, want 0", initial)
	}

	const n = 3
	for i := 0; i < n; i++ {
		if err := p.Begin(); err != nil {
			t.Fatal(err)
		}
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		p.MarkDirty(page.ID)
		if err := p.Commit(); err != nil {
			t.Fatal(err)
		}

		got, err := p.changeCounterOnDisk()
		if err != nil {
			t.Fatal(err)
		}
		if want := uint32(i + 1); got != want {
			t.Fatalf("%d 回目のコミット後の変更カウンタ = %d, want %d", i+1, got, want)
		}
	}

	before, err := p.changeCounterOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(headerPageID); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := p.changeCounterOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("読み取りだけの Tx で変更カウンタが変化しました: before=%d, after=%d", before, after)
	}
}
