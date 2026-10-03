// Command app is the e2e test workload: / answers hostname and VERSION,
// /health fails on demand, /slow?s=N holds a request open.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	host, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s\n", host, os.Getenv("VERSION"))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat("/tmp/unhealthy"); err == nil || os.Getenv("UNHEALTHY") == "1" {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		s, _ := strconv.Atoi(r.URL.Query().Get("s"))
		time.Sleep(time.Duration(s) * time.Second)
		fmt.Fprintf(w, "%s %s\n", host, os.Getenv("VERSION"))
	})
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, os.Getenv(r.URL.Query().Get("k")))
	})
	srv := &http.Server{Addr: ":8080", Handler: mux}
	if p := os.Getenv("PORT"); p != "" {
		srv.Addr = ":" + p
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	// Close the listener, finish in-flight requests, exit.
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Fatal(err)
	}
}
