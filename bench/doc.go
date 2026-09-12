// Package bench 是 pulse-web 的性能回归基线。
//
// 它测量两件事：
//  1. 框架请求路径的真实开销（作用域生命周期、事件派发、响应写出）；
//  2. kernel 在请求场景下的成本边界——哪些便宜（scope 派生、EmitLocal），
//     哪些是装配期的东西不该进请求路径（每请求 Provide / 全树 Emit）。
//
// 运行：
//
//	go test -bench . -benchmem ./bench/
//
// 结论随设计文档维护：docs/design/web-framework-design.md「实测数据」一节。
// 回归要求：请求路径开销不劣化（默认路径零 Provide，不随插件树规模线性增长）。
//
// 子目录 muxprobe 是 stdlib ServeMux 的能力探针（一次性工具，结论已写进设计文档）。
package bench
