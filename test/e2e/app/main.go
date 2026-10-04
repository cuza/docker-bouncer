// Command app is the e2e test workload: / answers hostname and VERSION,
// /health fails on demand, /slow?s=N holds a request open, /headers echoes
// the request headers, Host and RemoteAddr as JSON, /ws accepts any HTTP
// upgrade (a WebSocket handshake when asked) and echoes the bytes it gets.
package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
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
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"header": r.Header, "host": r.Host, "remote_addr": r.RemoteAddr})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: %s\r\nConnection: Upgrade\r\n", r.Header.Get("Upgrade"))
		if key := r.Header.Get("Sec-WebSocket-Key"); key != "" { // RFC 6455 section 4.2.2
			sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		}
		fmt.Fprint(rw, "\r\n")
		rw.Flush()
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			if err != nil {
				return
			}
			conn.Write(buf[:n])
		}
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
