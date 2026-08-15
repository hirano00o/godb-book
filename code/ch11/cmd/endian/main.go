// cmd/endian/main.go
// 数値がバイト列としてどう格納されるかを確かめる実験。
package main

import (
	"encoding/binary"
	"fmt"
)

func main() {
	buf := make([]byte, 4)
	var n uint32 = 4096 // 16 進数では 0x00001000

	binary.BigEndian.PutUint32(buf, n)
	fmt.Printf("ビッグエンディアン   : % x\n", buf)

	binary.LittleEndian.PutUint32(buf, n)
	fmt.Printf("リトルエンディアン   : % x\n", buf)

	// 読み戻しも対にして行う
	binary.BigEndian.PutUint32(buf, n)
	fmt.Printf("読み戻し             : %d\n", binary.BigEndian.Uint32(buf))
}
