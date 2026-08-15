// lock.go は flock(2) による、複数プロセス(または同一プロセス内の複数
// Pager インスタンス)間の同時アクセス制御を担う。
//
// Unix 専用(syscall.Flock を直接使う)。Windows では動作しない。
package pager

import (
	"container/list"
	"errors"
	"syscall"
)

// ErrBusy は他の接続がデータベースを使用中で、ロックを取得できなかった
// ことを表す(SQLite の SQLITE_BUSY に相当)。
var ErrBusy = errors.New("データベースがロックされています(他の接続が使用中です)")

// flockNB は DB ファイルの fd に対して flock(2) をノンブロッキングモード
// (LOCK_NB)付きで呼び出す薄いラッパー。取得できなければ ErrBusy を返す。
//
// flock はプロセス単位ではなくオープンファイル記述(fd)単位のロックなので、
// 同一プロセス内であっても別々の fd(= 別々の Pager インスタンス)同士は
// きちんと競合する。これを利用して、テストは 2 プロセスを起動せずとも
// 1 プロセス内で 2 つの Pager を開くだけで競合を再現できる。
func (p *Pager) flockNB(how int) error {
	if err := syscall.Flock(int(p.file.Fd()), how|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrBusy
		}
		return err
	}
	return nil
}

// lockShared は共有ロック(LOCK_SH)を取得する。共有ロックは複数の接続が
// 同時に保持できる(読み取り同士は競合しない)。
func (p *Pager) lockShared() error {
	return p.flockNB(syscall.LOCK_SH)
}

// lockExclusive は排他ロック(LOCK_EX)を取得する。
//
// 既に共有ロックを保持している状態からこれを呼ぶ(SH → EX への昇格)場合、
// flock(2) には「共有ロックを保持したまま排他ロックへ昇格する」操作が
// ない。そのためカーネル内部では、いったん共有ロックを手放してから
// 改めて排他ロックを取り直す形になり、その一瞬の隙間に別の接続が
// 割り込んでロックを奪う可能性が理論上ある。SQLite が
// SHARED → RESERVED → EXCLUSIVE という 3 段階のロックを用意し、
// 書き込みを始める前にまず RESERVED を取って「これから書き込むつもりだ」
// という意図を早期に宣言しているのは、まさにこの隙間を塞ぐためである。
// minidb は SHARED/EXCLUSIVE の 2 段階しか持たず、この素朴さ
// (昇格の瞬間に理論上の競合窓が空くこと)を受け入れる。
func (p *Pager) lockExclusive() error {
	return p.flockNB(syscall.LOCK_EX)
}

// unlock は保持しているロック(共有・排他いずれも)を解放する。
func (p *Pager) unlock() error {
	return syscall.Flock(int(p.file.Fd()), syscall.LOCK_UN)
}

// invalidateCacheIfStale はディスク上の変更カウンタとメモリ上の値を比較し、
// 異なっていれば(= 他の接続がこの Begin より前にコミットしていた)
// キャッシュ全体を無効化する。
//
// 呼び出しは Begin が共有ロックを取得した直後に限られ、その時点では
// トランザクションはまだ始まっていない(dirty ページは 1 枚も存在しない)。
// そのためキャッシュを丸ごと空にしてヘッダページだけ読み直せば十分で、
// 部分的な差分マージのような複雑な処理は要らない。
func (p *Pager) invalidateCacheIfStale() error {
	onDisk, err := p.changeCounterOnDisk()
	if err != nil {
		return err
	}
	if onDisk == p.changeCounter {
		return nil
	}
	p.cache = make(map[PageID]*Page)
	p.lru = list.New()
	p.lruIndex = make(map[PageID]*list.Element)
	return p.loadHeader()
}
