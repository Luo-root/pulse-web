// Command server 按 -fw / -mode 起一个被测服务。
//
// 两侧共用这一份 main：**只让 handler 是变量**。不用 gin 的 Run()、
// 也不用 pulse-web 的 Run()——那会把各自的默认 server 配置引进对比
// （超时、优雅关闭、错误日志），测出来的就不是框架的请求路径了。
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/Luo-root/pulse-web/loadtest/ginapp"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
)

func main() {
	fw := flag.String("fw", "pulse", "被测框架：pulse | gin")
	mode := flag.String("mode", "bare", "档位：bare | obs")
	addr := flag.String("addr", "127.0.0.1:18080", "监听地址")
	flag.Parse()

	var h http.Handler
	switch *fw {
	case "pulse":
		h = pulseapp.New(pulseapp.Mode(*mode))
	case "gin":
		h = ginapp.New(ginapp.Mode(*mode))
	default:
		log.Fatalf("未知 -fw %q", *fw)
	}

	// 同一份 bootstrap：net/http 默认配置，两侧完全一致。
	if err := http.ListenAndServe(*addr, h); err != nil {
		log.Fatal(err)
	}
}
