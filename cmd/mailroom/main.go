package main

import (
	"context"
	"log"
	"github.com/DengNaichen/mailroom/internal/app"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.Ltime)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c := app.ConfigFromEnv()
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if e := app.RunMCP(ctx, c); e != nil {
			log.Fatal(e)
		}
		return
	}
	if e := c.Validate(); e != nil {
		log.Fatal(e)
	}
	listener, e := net.Listen("tcp", c.Addr)
	if e != nil {
		log.Fatal(e)
	}
	defer listener.Close()
	a, e := app.New(c)
	if e != nil {
		log.Fatal(e)
	}
	defer a.Store.Close()
	if e = os.WriteFile(filepath.Join(c.DataDir, "mcp-token"), []byte(a.AgentToken), 0600); e != nil {
		log.Fatal(e)
	}
	server := &http.Server{Addr: c.Addr, Handler: a.Handler(ctx), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go a.Run(ctx)
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(stop)
	}()
	log.Printf("Mailroom → http://%s (demo=%t)", c.Addr, c.Demo)
	if e = server.Serve(listener); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
