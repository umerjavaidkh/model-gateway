// Command devredis runs an in-process Redis emulator on a port.
//
// It exists so the live check can exercise the Redis paths — rate limits and
// the usage stream — on a machine with no Redis and no container runtime. CI
// runs the same check against a real server, which is the gate; this is for
// the loop where waiting on CI to find a mistake is the slow way to work.
//
// It is a development tool and is never deployed.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alicebob/miniredis/v2"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6399", "address to listen on")
	flag.Parse()

	server := miniredis.NewMiniRedis()
	if err := server.StartAddr(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "devredis:", err)
		os.Exit(1)
	}
	defer server.Close()

	fmt.Printf("devredis listening on redis://%s\n", server.Addr())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
