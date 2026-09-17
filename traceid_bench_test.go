package web

import (
	crand "crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"testing"
	"time"
)

// trace-id 生成的三条实现对照（#78 的实测落点）。
//
// 结论是**不换随机源**（理由见设计文档「trace 头兼容与 span 出口」一节）：
// 三条实现的 allocs/op **完全相同**——那一次分配是返回的字符串本身，
// `hex.EncodeToString` 的中间 buffer 被编译器证明留在栈上，手写 nibble 也省不
// 出来。换 `math/rand/v2` 只省约 50 ns（占默认请求路径 ns 的 ~2.7%），却要放
// 弃「不可预测」，而那是 W3C 对 random-trace-id 的语义要求。
//
// 把**被否掉的方案**留在基准里，是为了下次有人再提「换个更快的随机源」时先看
// 这张表，而不是重新推一遍。
//
// 跑法（N 钉死，避免默认窗口让 N 随机器快慢漂）：
//
//	go test -run '^$' -bench BenchmarkGenerateTraceID -benchtime=200000x -count=5 .
//
// 关注 allocs/op；ns 只做量级参考。
func BenchmarkGenerateTraceID(b *testing.B) {
	b.Run("crypto+EncodeToString", func(b *testing.B) { benchTraceID(b, generateTraceID) })
	b.Run("crypto+manualHex", func(b *testing.B) { benchTraceID(b, traceIDCryptoManualHex) })
	b.Run("mathrand+manualHex-rejected", func(b *testing.B) { benchTraceID(b, traceIDMathRandManualHex) })
}

func benchTraceID(b *testing.B, fn func() string) {
	b.ReportAllocs()
	var s string
	for i := 0; i < b.N; i++ {
		s = fn()
	}
	traceIDSink = s // 防止整个调用被优化掉
}

var traceIDSink string

// traceIDCryptoManualHex 是「保留 crypto/rand、手工写十六进制」的对照实现
// （`hex.EncodeToString` 的替代），实测与现状同为 1 alloc / 32 B——所以现状
// 不用改。
func traceIDCryptoManualHex() string {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return fallbackTraceID()
	}
	var out [32]byte
	const hexDigits = "0123456789abcdef"
	for i, v := range raw {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out[:])
}

// traceIDMathRandManualHex 是**已否决**的备选源：更快，但不是密码学安全的随
// 机源（`math/rand/v2` 明文不用于安全用途），而 W3C 要求 trace-id 不可预测。
func traceIDMathRandManualHex() string {
	hi, lo := mrand.Uint64(), mrand.Uint64()
	var out [32]byte
	const hexDigits = "0123456789abcdef"
	for i := 0; i < 16; i++ {
		var v byte
		if i < 8 {
			v = byte(hi >> (56 - 8*i))
		} else {
			v = byte(lo >> (56 - 8*(i-8)))
		}
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out[:])
}

// fallbackTraceID 与 generateTraceID 的兜底同源（随机源不可用时）。
func fallbackTraceID() string { return fmt.Sprintf("%032x", time.Now().UnixNano()) }
