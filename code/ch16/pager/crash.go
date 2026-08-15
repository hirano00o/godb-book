// crash.go は障害注入(fault injection)のためのフックを提供する。
// クラッシュ実験(cmd/crashdemo)やテスト(crash_test.go)が、コミット
// プロトコルの特定の瞬間にプロセスを止め、クラッシュを決定的に
// 再現するために使う。通常運転では debugCrashHook は nil のままで、
// crashPoint の呼び出しは何もしない。
package pager

// debugCrashHook は障害注入フック。テストやデモが特定の瞬間にプロセスを
// 即死させ、クラッシュを決定的に再現するために使う。通常運転では nil の
// まま何もしない。
//
// os.Exit による即死は defer も Close も走らせない — kill -9 で殺された
// プロセスとディスク上の状態は区別がつかない。
var debugCrashHook func(point string)

// crashPoint は名前付きの注入点。debugCrashHook が設定されていれば呼ぶ。
func crashPoint(point string) {
	if debugCrashHook != nil {
		debugCrashHook(point)
	}
}

// SetDebugCrashHook は障害注入フックを設定する(デモ・テスト用)。
func SetDebugCrashHook(fn func(point string)) { debugCrashHook = fn }
