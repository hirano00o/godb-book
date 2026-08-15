// Package pager は minidb のストレージ層の最下層、
// 「ページ単位のファイル入出力とキャッシュ」を担当する。
package pager

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
)

const (
	// PageSize は 1 ページのバイト数。SQLite のデフォルトに合わせる。
	PageSize = 4096

	// Magic は minidb ファイルの先頭 8 バイト。
	Magic = "minidb01"

	// headerPageID はメタ情報を置くページ。常にファイルの先頭。
	headerPageID PageID = 0

	// defaultMaxCached はキャッシュに保持するページ数のデフォルト上限。
	// これを超えたら、dirty でないページを LRU 順(最も長く参照されて
	// いないもの)に 1 枚だけ追い出す。
	defaultMaxCached = 64
)

// PageID はページ番号。ページ n はファイルの n*PageSize バイト目から始まる。
type PageID uint32

// Page はメモリ上に読み込まれた 1 ページ。
type Page struct {
	ID    PageID
	Data  [PageSize]byte
	dirty bool // ディスクに書き戻す必要があるか
}

// Pager はページの読み書きとキャッシュを担当する。
// 上位層(B-Tree)はファイルオフセットを一切意識せず、
// PageID だけでページを取得・更新する。
//
// キャッシュは無制限に膨らまないよう LRU(Least Recently Used)で
// 上限を管理する。lru はページ参照の新しい順を保つ双方向リストで、
// 先頭が最も新しく参照されたページ、末尾が最も長く参照されていない
// ページを表す。lruIndex は PageID からリスト要素への逆引き。
type Pager struct {
	file     *os.File
	path     string           // DB ファイルのパス(ジャーナルファイル名の組み立てに使う)
	numPages uint32           // ヘッダページ含む総ページ数
	cache    map[PageID]*Page // 読み込み済みページのキャッシュ

	lru       *list.List
	lruIndex  map[PageID]*list.Element
	maxCached int

	inTx bool // トランザクション中かどうか(Begin〜Commit/Rollback)
}

// Open は minidb ファイルを開く。存在しなければ新規作成し、
// ヘッダページ(ページ 0)を初期化する。
//
// 既存ファイルを開く際は、ヘッダページを検証するより先に
// recoverFromJournal でコミット未完了のジャーナルがないか確認する
// (ヘッダページ自体が復元対象になっていることがあるため)。
func Open(path string) (*Pager, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	p := &Pager{
		file:      file,
		path:      path,
		cache:     make(map[PageID]*Page),
		lru:       list.New(),
		lruIndex:  make(map[PageID]*list.Element),
		maxCached: defaultMaxCached,
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	if info.Size() == 0 {
		// 新規ファイル: ヘッダページを作って書き込む
		if err := p.initHeader(); err != nil {
			file.Close()
			return nil, err
		}
		return p, nil
	}

	if err := p.recoverFromJournal(p.journalPath()); err != nil {
		file.Close()
		return nil, err
	}

	// 既存ファイル: ヘッダページを検証して総ページ数を読み取る
	if err := p.loadHeader(); err != nil {
		file.Close()
		return nil, err
	}
	return p, nil
}

// SetCacheLimit はキャッシュに保持するページ数の上限を変更する。
// テストで小さい値を設定し、意図的に追い出しを発生させるために公開している。
func (p *Pager) SetCacheLimit(n int) {
	p.maxCached = n
}

// initHeader はヘッダページを初期化してディスクに書き込む。
// レイアウト:
//
//	オフセット 0-7  : マジック文字列 "minidb01"
//	オフセット 8-11 : ページサイズ(ビッグエンディアン uint32)
//	オフセット 12-15: 総ページ数(ビッグエンディアン uint32)
//	オフセット 16-19: ルートページ番号(ビッグエンディアン uint32)
//	オフセット 20-23: フリーリスト先頭ページ番号(ビッグエンディアン uint32、0=空)
//	オフセット 24-27: フリーリストのページ数(ビッグエンディアン uint32)
func (p *Pager) initHeader() error {
	page := &Page{ID: headerPageID}
	copy(page.Data[0:8], Magic)
	binary.BigEndian.PutUint32(page.Data[8:12], PageSize)

	p.numPages = 1 // ヘッダページ自身
	binary.BigEndian.PutUint32(page.Data[12:16], p.numPages)

	p.cache[headerPageID] = page
	p.touch(headerPageID)
	page.dirty = true
	return p.Flush()
}

// loadHeader は既存ファイルのヘッダページを読み込んで検証する。
func (p *Pager) loadHeader() error {
	page, err := p.readPage(headerPageID)
	if err != nil {
		return err
	}
	if string(page.Data[0:8]) != Magic {
		return errors.New("minidb ファイルではありません")
	}
	if got := binary.BigEndian.Uint32(page.Data[8:12]); got != PageSize {
		return fmt.Errorf("ページサイズが不一致: file=%d, build=%d", got, PageSize)
	}
	p.numPages = binary.BigEndian.Uint32(page.Data[12:16])
	p.cache[headerPageID] = page
	p.touch(headerPageID)
	return nil
}

// NumPages はヘッダページを含む総ページ数を返す。
func (p *Pager) NumPages() uint32 { return p.numPages }

// Get はページを取得する。キャッシュにあればそれを、
// なければディスクから読み込んで返す。
func (p *Pager) Get(id PageID) (*Page, error) {
	if uint32(id) >= p.numPages {
		return nil, fmt.Errorf("ページ %d は存在しません(総ページ数 %d)", id, p.numPages)
	}
	if page, ok := p.cache[id]; ok {
		p.touch(id)
		return page, nil
	}
	page, err := p.readPage(id)
	if err != nil {
		return nil, err
	}
	p.cache[id] = page
	p.touch(id)
	return page, nil
}

// Allocate はページを 1 枚確保して返す。フリーリストにページがあれば
// そこから再利用し(ファイルは伸びない)、空ならファイル末尾に新しい
// ページを追加する。ディスクへの書き込みは Flush まで遅延される。
func (p *Pager) Allocate() (*Page, error) {
	header, err := p.Get(headerPageID)
	if err != nil {
		return nil, err
	}
	head := PageID(binary.BigEndian.Uint32(header.Data[freelistHeadOffset : freelistHeadOffset+4]))
	if head != 0 {
		return p.allocateFromFreelist(head)
	}
	return p.allocateNew()
}

// allocateNew はファイル末尾に新しいページを追加する。
func (p *Pager) allocateNew() (*Page, error) {
	page := &Page{ID: PageID(p.numPages), dirty: true}
	p.numPages++
	p.cache[page.ID] = page
	p.touch(page.ID)

	// ヘッダページの総ページ数も更新する
	header, err := p.Get(headerPageID)
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(header.Data[12:16], p.numPages)
	header.dirty = true
	return page, nil
}

// allocateFromFreelist はフリーリスト先頭のページ head を取り出して再利用する。
func (p *Pager) allocateFromFreelist(head PageID) (*Page, error) {
	page, err := p.Get(head)
	if err != nil {
		return nil, err
	}
	next := binary.BigEndian.Uint32(page.Data[0:4])
	// この直後の Get(headerPageID) で追い出しが起こっても page 自身が
	// 消えないよう、内容を書き換える前に dirty にしておく。
	p.MarkDirty(head)

	header, err := p.Get(headerPageID)
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(header.Data[freelistHeadOffset:freelistHeadOffset+4], next)
	count := binary.BigEndian.Uint32(header.Data[freelistCountOffset : freelistCountOffset+4])
	binary.BigEndian.PutUint32(header.Data[freelistCountOffset:freelistCountOffset+4], count-1)
	header.dirty = true

	page.Data = [PageSize]byte{}
	return page, nil
}

// Free はページをフリーリストへ返却する。ページの内容は破棄され、
// 先頭 4 バイトが「次の空きページ番号」として使われる。
func (p *Pager) Free(id PageID) error {
	page, err := p.Get(id)
	if err != nil {
		return err
	}
	// この直後の Get(headerPageID) で追い出しが起こっても page 自身が
	// 消えないよう、内容を書き換える前に dirty にしておく。
	p.MarkDirty(id)
	page.Data = [PageSize]byte{}

	header, err := p.Get(headerPageID)
	if err != nil {
		return err
	}
	oldHead := binary.BigEndian.Uint32(header.Data[freelistHeadOffset : freelistHeadOffset+4])
	binary.BigEndian.PutUint32(page.Data[0:4], oldHead)

	binary.BigEndian.PutUint32(header.Data[freelistHeadOffset:freelistHeadOffset+4], uint32(id))
	count := binary.BigEndian.Uint32(header.Data[freelistCountOffset : freelistCountOffset+4])
	binary.BigEndian.PutUint32(header.Data[freelistCountOffset:freelistCountOffset+4], count+1)
	header.dirty = true
	return nil
}

// MarkDirty はページの内容を書き換えたことをページャに伝える。
// 上位層は Data を書き換える前に必ずこれを呼ぶこと。dirty なページは
// LRU 追い出しの対象から外れるので、特に「書き換えの途中で Get や
// Allocate を呼ぶ」場合(その間にキャッシュの追い出しが起こりうる)は、
// 書き換え対象のページを先に MarkDirty しておくことが必須になる。
// これを怠ると、書き換え途中のページがキャッシュから追い出され、
// 変更が失われる可能性がある。
func (p *Pager) MarkDirty(id PageID) {
	if page, ok := p.cache[id]; ok {
		page.dirty = true
	}
}

// touch はページ id への参照を LRU の先頭(最も新しい位置)に記録し、
// キャッシュ収容数が上限を超えていれば 1 枚だけ追い出しを試みる。
func (p *Pager) touch(id PageID) {
	if elem, ok := p.lruIndex[id]; ok {
		p.lru.MoveToFront(elem)
	} else {
		p.lruIndex[id] = p.lru.PushFront(id)
	}
	p.evictIfNeeded(id)
}

// evictIfNeeded はキャッシュ収容数が上限を超えている場合、LRU の末尾から
// 辿って最初に見つかった dirty でないページを 1 枚追い出す。dirty な
// ページは書き換え中(または未書き戻し)なので絶対に追い出さない。
// また justUsed(たった今参照され、これから呼び出し側に渡るページ)も
// 追い出さない — 他の全ページが dirty のとき、追い出せる唯一の clean な
// ページが「今まさに返そうとしているページ自身」になってしまうため。
// すべて追い出せないなら何もしない(あくまでソフトな上限)。
func (p *Pager) evictIfNeeded(justUsed PageID) {
	if len(p.cache) <= p.maxCached {
		return
	}
	for e := p.lru.Back(); e != nil; e = e.Prev() {
		id := e.Value.(PageID)
		if id == justUsed {
			continue
		}
		page, ok := p.cache[id]
		if !ok || page.dirty {
			continue
		}
		delete(p.cache, id)
		delete(p.lruIndex, id)
		p.lru.Remove(e)
		return
	}
}

// Flush はダーティなページをすべてディスクに書き戻し、
// fsync でストレージへの到達を保証する。
//
// トランザクション中は呼び出せない。トランザクション中にディスクへ
// 書き戻してよいのは Commit(ジャーナルへの退避を終えた後)だけであり、
// これを守ることでコミット前の変更が DB 本体に漏れ出さないようにしている。
func (p *Pager) Flush() error {
	if p.inTx {
		return errors.New("トランザクション中は Flush できません(Commit を使ってください)")
	}
	return p.flush()
}

// flush は dirty ページを実際にディスクへ書き戻す内部処理。
// トランザクション中かどうかを検査しないので、Commit など
// トランザクション制御の内部からのみ呼び出すこと。
func (p *Pager) flush() error {
	for _, page := range p.cache {
		if !page.dirty {
			continue
		}
		if err := p.writePage(page); err != nil {
			return err
		}
		page.dirty = false
	}
	// OS のバッファに残っているデータを物理ディスクまで書かせる。
	// これを省くと、電源断でファイルが壊れる可能性がある。
	return p.file.Sync()
}

// dirtyPages はキャッシュ中の dirty なページを PageID 昇順で返す。
// 順序を固定するのは、ジャーナルの内容を再現可能にするため。
func (p *Pager) dirtyPages() []*Page {
	var dirty []*Page
	for _, page := range p.cache {
		if page.dirty {
			dirty = append(dirty, page)
		}
	}
	sort.Slice(dirty, func(i, j int) bool { return dirty[i].ID < dirty[j].ID })
	return dirty
}

// Close は Flush してからファイルを閉じる。トランザクション中であれば、
// コミットされていない変更を確定させてしまわないよう、まず自動的に
// Rollback してから閉じる。
func (p *Pager) Close() error {
	if p.inTx {
		if err := p.Rollback(); err != nil {
			p.file.Close()
			return err
		}
	}
	if err := p.Flush(); err != nil {
		p.file.Close()
		return err
	}
	return p.file.Close()
}

// Begin はトランザクションを開始する。以後、Commit か Rollback まで
// ディスクへの書き戻し(Flush)は行われない。二重 Begin はエラー。
func (p *Pager) Begin() error {
	if p.inTx {
		return errors.New("トランザクションは既に開始しています")
	}
	p.inTx = true
	return nil
}

// InTx はトランザクション中かどうかを返す。
func (p *Pager) InTx() bool { return p.inTx }

// Commit は変更前ページをジャーナルへ退避してから dirty ページを
// ディスクへ書き戻し、最後にジャーナルを削除して変更を確定する。
//
// dirty ページが 1 枚もなければ(変更のないトランザクション)、
// ジャーナルを作ることなくそのままトランザクションを終える。
func (p *Pager) Commit() error {
	if !p.inTx {
		return errors.New("トランザクションが開始されていません")
	}

	dirty := p.dirtyPages()
	if len(dirty) == 0 {
		p.inTx = false
		return nil
	}

	if err := p.writeJournal(dirty); err != nil {
		return err
	}
	if err := p.flush(); err != nil {
		return err
	}
	// ジャーナルの削除こそが「コミットの瞬間」。ここまでの手順(ジャーナル
	// 書き込み → fsync → ヘッダ書き込み → fsync → DB 本体へ Flush)が
	// すべて終わって初めて安全に消せる。
	if err := os.Remove(p.journalPath()); err != nil {
		return err
	}

	p.inTx = false
	return nil
}

// Rollback は変更をすべて捨てる。dirty ページをキャッシュ(と LRU)から
// 取り除き、ディスク上のコミット済みヘッダページを読み直して numPages を
// 復元する。トランザクション中はディスクへ一切書いていないので、
// 消すべきジャーナルもない。
func (p *Pager) Rollback() error {
	if !p.inTx {
		return errors.New("トランザクションが開始されていません")
	}

	for id, page := range p.cache {
		if !page.dirty {
			continue
		}
		delete(p.cache, id)
		if elem, ok := p.lruIndex[id]; ok {
			p.lru.Remove(elem)
			delete(p.lruIndex, id)
		}
	}

	// ヘッダページ自身が dirty だった場合は上のループで既にキャッシュから
	// 落ちている。ディスク上の(トランザクション開始前のままの)内容を
	// 直接読み直し、numPages を復元する。これにより Allocate による
	// メモリ上の割り当ても巻き戻る。
	header, err := p.readPage(headerPageID)
	if err != nil {
		return err
	}
	p.numPages = binary.BigEndian.Uint32(header.Data[12:16])

	p.inTx = false
	return nil
}

// readPage はディスクからページを 1 枚読み込む(キャッシュを介さない)。
func (p *Pager) readPage(id PageID) (*Page, error) {
	page := &Page{ID: id}
	_, err := p.file.ReadAt(page.Data[:], int64(id)*PageSize)
	if err != nil {
		return nil, fmt.Errorf("ページ %d の読み取りに失敗: %w", id, err)
	}
	return page, nil
}

// writePage はページをディスク上の正しい位置に書き込む。
func (p *Pager) writePage(page *Page) error {
	_, err := p.file.WriteAt(page.Data[:], int64(page.ID)*PageSize)
	if err != nil {
		return fmt.Errorf("ページ %d の書き込みに失敗: %w", page.ID, err)
	}
	return nil
}

// ルートページ番号はヘッダページのオフセット 16-19 に記録する。
// 0 は「B-Tree がまだ作られていない」ことを表す。
const rootPageOffset = 16

// RootPage はヘッダページに記録された B-Tree のルートページ番号を返す。
func (p *Pager) RootPage() (PageID, error) {
	header, err := p.Get(headerPageID)
	if err != nil {
		return 0, err
	}
	return PageID(binary.BigEndian.Uint32(header.Data[rootPageOffset : rootPageOffset+4])), nil
}

// SetRootPage はルートページ番号をヘッダページに記録する。
func (p *Pager) SetRootPage(id PageID) error {
	header, err := p.Get(headerPageID)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(header.Data[rootPageOffset:rootPageOffset+4], uint32(id))
	header.dirty = true
	return nil
}

// フリーリスト先頭ページ番号とページ数は、ヘッダページの
// オフセット 20-23, 24-27 に記録する。先頭 = 0 は「フリーリストが空」を表す。
// 解放されたページはオフセット 0-3 に「次の空きページ番号」を持つ
// 連結リストになる(SQLite のフリーリストの最小版。トランクページは作らない)。
const (
	freelistHeadOffset  = 20
	freelistCountOffset = 24
)

// FreelistHead はフリーリスト先頭のページ番号を返す(空なら 0)。
func (p *Pager) FreelistHead() PageID {
	header, err := p.Get(headerPageID)
	if err != nil {
		return 0
	}
	return PageID(binary.BigEndian.Uint32(header.Data[freelistHeadOffset : freelistHeadOffset+4]))
}

// FreelistCount はフリーリストに繋がっているページ数を返す。
func (p *Pager) FreelistCount() uint32 {
	header, err := p.Get(headerPageID)
	if err != nil {
		return 0
	}
	return binary.BigEndian.Uint32(header.Data[freelistCountOffset : freelistCountOffset+4])
}
