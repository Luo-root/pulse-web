// Command loadgen 是压测器的独立入口：对着一个**已经在跑**的服务打，
// 用来手工复核、或在不跑完整对比时单测某一档。
//
// 完整对比（自己起服务、跑完所有档位）见 cmd/compare。
package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/Luo-root/pulse-web/loadtest/internal/loadgen"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:18080/users/42", "压测目标")
	c := flag.Int("c", 64, "并发连接数")
	d := flag.Duration("d", 15*time.Second, "计时时长")
	warmup := flag.Duration("warmup", 3*time.Second, "预热时长（结果丢弃）")
	flag.Parse()

	fmt.Println(loadgen.Run(loadgen.Config{
		URL:         *url,
		Concurrency: *c,
		Warmup:      *warmup,
		Duration:    *d,
	}).String())
}
