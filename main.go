package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfgPath := flag.String("config", "", "path to config.json (optional, defaults are fine)")
	listen := flag.String("listen", "", "override listen address, e.g. 0.0.0.0:8080")
	selfTest := flag.Bool("selftest", false, "probe zen free tier with the cloned recipe and exit")
	flag.Parse()

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *selfTest {
		os.Exit(runSelfTest(cfg))
	}
	// pull the live zero-cost opencode (zen) catalog; non-fatal on failure
	EnrichZen(cfg)

	srv := newServer(cfg)

	httpSrv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: srv.routes(),
	}

	go func() {
		fmt.Printf("opencode-free-proxy listening on http://%s\n", cfg.Server.Listen)
		log.Printf("enabled providers: %s", enabledList(cfg))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	fmt.Println("\nshutting down…")
	_ = httpSrv.Close()
}

func enabledList(cfg *Config) (out []string) {
	for _, p := range cfg.Providers {
		if p.Enabled() {
			out = append(out, p.Name)
		}
	}
	if len(out) == 0 {
		return []string{"none (set OPENROUTER_API_KEY, GROQ_API_KEY, or GEMINI_API_KEY)"}
	}
	return out
}
