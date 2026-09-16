// Package bench 量化「同一个请求路径，两边各付多少」。
//
// 这是**成本分解**，不是胜负承诺——验收标准原文就是「不以 micro-benchmark
// 胜负作承诺」。真实负载的结论看 loadtest 的主对比（cmd/compare）。
//
// 跑法：
//
//	go test -bench . -benchmem ./bench/
package bench
