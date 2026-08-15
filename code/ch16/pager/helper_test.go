package pager

import "os"

// writeJunk はテスト用に PageSize バイトのでたらめな内容のファイルを作る。
func writeJunk(path string) error {
	junk := make([]byte, PageSize)
	copy(junk, "this is not a minidb file")
	return os.WriteFile(path, junk, 0o644)
}
