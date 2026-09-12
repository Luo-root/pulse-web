// muxprobe 验证 stdlib ServeMux（Go 1.22+）的真实能力边界，用于评估
// "路由用 ServeMux" 这条设计决策的取舍。一次性探针，测完即弃。
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

func probe(mux *http.ServeMux, method, path string) {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Printf("  %-6s %-22s -> %d %s %s\n", method, path, rec.Code, rec.Header().Get("Location"), rec.Body.String())
}

func main() {
	// --- 1. 冲突注册：是 panic 还是静默覆盖 ---
	fmt.Println("[1] 冲突注册 {x} vs {y}:")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("  panic:", r)
			}
		}()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /a/{x}", func(w http.ResponseWriter, _ *http.Request) {})
		mux.HandleFunc("GET /a/{y}", func(w http.ResponseWriter, _ *http.Request) {})
		fmt.Println("  没有 panic —— 静默接受")
	}()

	// --- 2. 优先级：字面量 vs 通配 ---
	fmt.Println("[2] 优先级 (字面量 /a/literal vs 通配 /a/{x}):")
	mux2 := http.NewServeMux()
	mux2.HandleFunc("GET /a/{x}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "wildcard:", r.PathValue("x")) })
	mux2.HandleFunc("GET /a/literal", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "literal") })
	probe(mux2, "GET", "/a/literal")
	probe(mux2, "GET", "/a/other")

	// --- 3. 方法限定 vs 通用 ---
	fmt.Println("[3] 方法限定 (GET /m 与 /m 无方法):")
	mux3 := http.NewServeMux()
	mux3.HandleFunc("GET /m", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "get-only") })
	mux3.HandleFunc("/m", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "any-method") })
	probe(mux3, "GET", "/m")
	probe(mux3, "POST", "/m")

	// --- 4. 尾斜杠自动重定向（经典困惑源） ---
	fmt.Println("[4] 尾斜杠行为 (/dir/ 注册后访问 /dir):")
	mux4 := http.NewServeMux()
	mux4.HandleFunc("GET /dir/{rest...}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "rest=", r.PathValue("rest")) })
	probe(mux4, "GET", "/dir/a/b")
	probe(mux4, "GET", "/dir/")
	probe(mux4, "GET", "/dir")

	// --- 5. {rest...} 能否匹配空段 ---
	fmt.Println("[5] 通配后缀匹配空值 (/files/{rest...}):")
	mux5 := http.NewServeMux()
	mux5.HandleFunc("GET /files/{rest...}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "rest=%q", r.PathValue("rest")) })
	probe(mux5, "GET", "/files/")
	probe(mux5, "GET", "/files")
	probe(mux5, "GET", "/files/a.txt")

	// --- 6. 单段通配能否匹配空段 ---
	fmt.Println("[6] 单段通配匹配空值 (GET /u/{id}):")
	mux6 := http.NewServeMux()
	mux6.HandleFunc("GET /u/{id}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "id=%q", r.PathValue("id")) })
	probe(mux6, "GET", "/u/")
	probe(mux6, "GET", "/u/42")

	// --- 7. 正则约束是否支持 ---
	fmt.Println("[7] 正则约束 {id:[0-9]+}:")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("  panic:", r)
			}
		}()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /n/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request) {})
		fmt.Println("  被接受了")
	}()

	// --- 8. Static 风格注册：/static/ 前缀 vs /static/{file...} 是否冲突 ---
	fmt.Println("[8] Static 模式冲突 (/static/ vs GET /static/{file...}):")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("  panic:", r)
			}
		}()
		mux := http.NewServeMux()
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("."))))
		mux.HandleFunc("GET /static/{file...}", func(w http.ResponseWriter, _ *http.Request) {})
		fmt.Println("  没有 panic —— 可以共存")
	}()

	// --- 9. 纯前缀模式（尾斜杠）的匹配范围与重定向 ---
	fmt.Println("[9] 前缀模式 /static/ 的匹配范围:")
	mux9 := http.NewServeMux()
	mux9.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "prefix-hit path=", r.URL.Path) })
	probe(mux9, "GET", "/static/a/b.txt")
	probe(mux9, "GET", "/static/")
	probe(mux9, "GET", "/static")

	// --- 10. 方法限定的前缀模式 ---
	fmt.Println("[10] 方法限定前缀模式 (GET /assets/):")
	mux10 := http.NewServeMux()
	mux10.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "get-prefix path=", r.URL.Path) })
	probe(mux10, "GET", "/assets/x.css")
	probe(mux10, "POST", "/assets/x.css")
}
