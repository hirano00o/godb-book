// catalog.go は minidb のテーブル定義一覧を管理するカタログを担当する。
// SQLite の sqlite_schema テーブルの minidb 版に相当する。
package exec

import (
	"errors"
	"fmt"

	"minidb/btree"
	"minidb/pager"
	"minidb/sql"
)

var (
	ErrTableNotFound = errors.New("テーブルが見つかりません")
	ErrTableExists   = errors.New("テーブルは既に存在します")
)

// TableInfo はカタログに登録されたテーブル 1 つの情報。
type TableInfo struct {
	ID   uint64 // カタログ内のキー
	Name string
	Root pager.PageID // テーブルの木のルートページ(分割・収縮で動く)

	Columns []sql.ColumnDef // CREATE TABLE 文を再パースして得る
	PKIndex int             // INTEGER PRIMARY KEY 列の位置。なければ -1
}

// Catalog はテーブル定義の一覧を管理する。
//
// 実体は特別なテーブルではなく、カタログ自身が 1 つの B-Tree で、
// キーはテーブル ID(挿入順の連番)、値は (テーブル名, ルートページ,
// CREATE TABLE 文)をエンコードしたレコードである。この木のルートは
// ヘッダページが指す、pager にとって唯一特別な木になる
// (テーブルの木は btree.OpenAt でカタログ経由でしか開けない)。
type Catalog struct {
	pg   *pager.Pager
	tree *btree.BTree

	nextID uint64
}

// OpenCatalog はヘッダページのルートが指すカタログの木を開く。
// まだ木が存在しなければ btree.Open が空の木を新規に作る。
func OpenCatalog(pg *pager.Pager) (*Catalog, error) {
	tree, err := btree.Open(pg)
	if err != nil {
		return nil, err
	}
	c := &Catalog{pg: pg, tree: tree}

	// 次に割り当てるテーブル ID は「現在の最大キー + 1」。カタログの木は
	// 行の削除(DROP TABLE 相当)を今のところ持たないので、MaxKey で
	// 十分足りる。
	maxKey, ok, err := tree.MaxKey()
	if err != nil {
		return nil, err
	}
	if ok {
		c.nextID = maxKey + 1
	}
	return c, nil
}

// CreateTable は CREATE TABLE 文からテーブルを作る。名前の重複はエラー。
func (c *Catalog) CreateTable(stmt *sql.CreateTableStmt, sqlText string) error {
	if _, err := c.Get(stmt.Table); err == nil {
		return fmt.Errorf("%w: %s", ErrTableExists, stmt.Table)
	} else if !errors.Is(err, ErrTableNotFound) {
		return err
	}

	table, err := btree.Create(c.pg)
	if err != nil {
		return err
	}

	id := c.nextID
	c.nextID++

	value, err := encodeCatalogRow(stmt.Table, table.Root(), sqlText)
	if err != nil {
		return err
	}
	return c.tree.Insert(id, value)
}

// Get は名前でテーブルを引く。見つからなければ ErrTableNotFound。
func (c *Catalog) Get(name string) (*TableInfo, error) {
	var found *TableInfo
	err := c.tree.Scan(func(key uint64, value []byte) error {
		info, err := decodeCatalogRow(key, value)
		if err != nil {
			return err
		}
		if info.Name == name {
			found = info
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	return found, nil
}

// List は全テーブルを ID 順に返す(.tables 用)。
// カタログの木は ID(挿入順の連番)をキーにしているので、Scan が
// そのまま昇順(= ID 順)で返す。
func (c *Catalog) List() ([]*TableInfo, error) {
	var infos []*TableInfo
	err := c.tree.Scan(func(key uint64, value []byte) error {
		info, err := decodeCatalogRow(key, value)
		if err != nil {
			return err
		}
		infos = append(infos, info)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// UpdateRoot はテーブルの木のルートページ番号を書き換える
// (テーブルへの書き込みでルートが動いたときに呼ぶ)。
//
// カタログの木は他の B-Tree と同じく「値の上書き」API を持たない
// (Insert は重複キーをエラーにする)。そのため、該当するカタログ行を
// 一度 Delete してから同じキーで Insert し直すことで更新を表現する。
func (c *Catalog) UpdateRoot(info *TableInfo, newRoot pager.PageID) error {
	if err := c.tree.Delete(info.ID); err != nil {
		return err
	}
	value, err := encodeCatalogRow(info.Name, newRoot, createTableText(info))
	if err != nil {
		return err
	}
	if err := c.tree.Insert(info.ID, value); err != nil {
		return err
	}
	info.Root = newRoot
	return nil
}

// encodeCatalogRow はカタログ 1 行分(テーブル名, ルートページ, CREATE
// TABLE 文)を EncodeRecord でバイト列にする。カタログ自身もテーブルの
// 木と同じレコード形式を流用する。
func encodeCatalogRow(name string, root pager.PageID, sqlText string) ([]byte, error) {
	return EncodeRecord([]sql.Value{
		{IsText: true, Text: name},
		{Int: int64(root)},
		{IsText: true, Text: sqlText},
	})
}

// decodeCatalogRow はカタログの 1 行を TableInfo に復元する。
//
// スキーマ(列定義)そのものはカタログに構造化して持たず、CREATE TABLE
// 文のテキストとして保存してあり、読み出すたびに sql.Parse で再解析する。
// これは SQLite の sqlite_schema がまさに採っている手口で、スキーマの
// 表現を 1 か所(パーサ)に集約できる利点がある。
func decodeCatalogRow(key uint64, value []byte) (*TableInfo, error) {
	values, err := DecodeRecord(value)
	if err != nil {
		return nil, err
	}
	if len(values) != 3 {
		return nil, fmt.Errorf("カタログの行の列数が不正です: %d", len(values))
	}
	name := values[0].Text
	root := pager.PageID(values[1].Int)
	sqlText := values[2].Text

	stmt, err := sql.Parse(sqlText)
	if err != nil {
		return nil, fmt.Errorf("カタログの CREATE TABLE 文の再解析に失敗しました: %w", err)
	}
	createStmt, ok := stmt.(*sql.CreateTableStmt)
	if !ok {
		return nil, fmt.Errorf("カタログの行が CREATE TABLE 文ではありません: %s", sqlText)
	}

	pkIndex := -1
	for i, col := range createStmt.Columns {
		if col.PrimaryKey {
			pkIndex = i
			break
		}
	}

	return &TableInfo{
		ID:      key,
		Name:    name,
		Root:    root,
		Columns: createStmt.Columns,
		PKIndex: pkIndex,
	}, nil
}

// createTableText は TableInfo から CREATE TABLE 文のテキストを組み立て
// 直す。UpdateRoot はルートページ番号だけを書き換えたいが、カタログの
// 行は (名前, ルート, SQL テキスト) の組でしか保存できないため、更新の
// たびに保存済みの列定義から SQL テキストを再構成する。
func createTableText(info *TableInfo) string {
	var b []byte
	b = append(b, "CREATE TABLE "...)
	b = append(b, info.Name...)
	b = append(b, " ("...)
	for i, col := range info.Columns {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, col.Name...)
		b = append(b, ' ')
		b = append(b, col.Type.String()...)
		if col.PrimaryKey {
			b = append(b, " PRIMARY KEY"...)
		}
	}
	b = append(b, ')')
	return string(b)
}
