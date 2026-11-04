// mcp/remote —— 运行一个绑在 127.0.0.1 的 Streamable HTTP MCP server，并用内置 client 自测一遍。
//
//	TOKEN=xxxx go run ./mcp/remote          # 默认端口 8765；TOKEN 缺省为 demo-token（仅供本地试跑）
//
// 对外提供时务必：强随机 TOKEN、HTTPS 反向代理、只绑内网地址。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	token := os.Getenv("TOKEN")
	if token == "" {
		token = "demo-token"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8765"
	}

	srv := &http.Server{Addr: addr, Handler: newStreamServer(token)}
	go func() {
		log.Printf("MCP endpoint: http://%s/mcp", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// 内置 client 自测：握手 → 工具调用 → 通知 → 关闭会话
	time.Sleep(300 * time.Millisecond)
	c := NewRemoteClient("http://"+addr+"/mcp", token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		log.Fatal("initialize: ", err)
	}
	res, err := c.Call(ctx, "tools/call", map[string]any{
		"name": "greet", "arguments": map[string]any{"name": "远程世界"},
	})
	if err != nil {
		log.Fatal("tools/call: ", err)
	}
	fmt.Println("tools/call 结果:", string(res))
	if err := c.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		log.Fatal("notify: ", err)
	}
	if err := c.Close(ctx); err != nil {
		log.Fatal("close: ", err)
	}
	fmt.Println("握手→调用→通知→关闭会话：全链路通过")
}
