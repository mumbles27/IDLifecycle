// idl-mockui serves the web console against the in-memory mock API (internal/web/testdata/mock) on the loopback
// address, for trying the UI without a server, a database or AD. Nothing it does reaches a directory or a tenant.
// It is a developer tool: release builds contain only cmd/idlifecycled.
//
//	go run ./cmd/idl-mockui            # from the repository root
//	go run ./cmd/idl-mockui -port 8099 -web internal/web
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	web := flag.String("web", filepath.Join("internal", "web"), "the internal/web folder of a checkout")
	port := flag.Int("port", 8099, "loopback port to listen on")
	flag.Parse()

	if _, err := os.Stat(filepath.Join(*web, "testdata", "mock", "mock-api.js")); err != nil {
		fmt.Fprintf(os.Stderr, "no mock API under %s (run from the repository root or pass -web)\n", *web)
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.Handle("/mock/", http.StripPrefix("/mock/", http.FileServer(http.Dir(filepath.Join(*web, "testdata", "mock")))))
	mux.Handle("/selftest/", http.StripPrefix("/selftest/", http.FileServer(http.Dir(filepath.Join(*web, "testdata", "selftest")))))
	// The real server picks the logo file; here it is always the built-in one.
	mux.HandleFunc("/branding/logo", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(*web, "static", "branding", "logo.svg"))
	})
	mux.Handle("/", http.FileServer(http.Dir(filepath.Join(*web, "static"))))

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	base := "http://" + ln.Addr().String()
	fmt.Printf("Mock console:   %s/mock/mock.html?autologin=admin#/dashboard\n", base)
	fmt.Printf("Mock selftest:  %s/mock/mock-selftest.html?user=admin&pass=admin-pass\n", base)
	fmt.Println("Mock sign-ins: admin / admin-pass, approver1 / approver-pass (see mock-api.js). Ctrl+C to stop.")
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
