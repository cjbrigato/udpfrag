# udpfrag - UDP Fragmentation and Reassembly Core for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/cjbrigato/udpfrag.svg)](https://pkg.go.dev/github.com/cjbrigato/udpfrag)
[![Go Report Card](https://goreportcard.com/badge/github.com/cjbrigato/udpfrag)](https://goreportcard.com/report/github.com/cjbrigato/udpfrag)


`udpfrag` is a Go package providing the core logic to overcome the typical size limitations of UDP datagrams (imposed by MTU - Maximum Transmission Unit). It automatically fragments large messages before sending and reassembles them upon reception.

This package provides:

1.  The low-level `FragmentData` and `ReassembleData` functions.
2.  A convenient `UDPClientConn` type (`net.Conn`-like) for *client-side* connections that handles fragmentation/reassembly transparently.

For a high-level *server-side* abstraction that mimics `net.Listener`, please see the companion package: [**`github.com/cjbrigato/udplistener`**](https://github.com/cjbrigato/udplistener). The `udplistener` package uses `udpfrag` internally.

## Features (udpfrag core)

*   **Core Fragmentation Logic:** Splits messages larger than the configured `MaxFragmentSize`.
*   **Core Reassembly Logic:** Reconstructs the original message from received fragments.
*   **`UDPClientConn`:** A `net.Conn`-like interface for **client** connections.
*   **Configurable:** Allows setting maximum fragment payload size, reassembly timeout, and cleanup interval (configuration applies globally and is used by `udplistener` as well).
*   **Resource Pooling:** Uses `sync.Pool` to reuse buffers, reducing GC pressure.
*   **Timeout Handling:** Automatically cleans up incomplete message buffers after a timeout.
*   **Conditional Logging:** Includes optional debug logging.

## Installation

```bash
# To get the core fragmentation logic and client helper
go get github.com/cjbrigato/udpfrag

# To get the server listener helper (which includes udpfrag)
go get github.com/cjbrigato/udplistener
```

## Usage

### Configuration (Applies to both `udpfrag` and `udplistener`)

Before using either the client or server helpers, you can configure the fragmentation parameters globally.

```go
package main

import (
	"time"
	"github.com/cjbrigato/udpfrag"
)

func main() {
    myConfig := udpfrag.Config{
        MaxFragmentSize:   1024,                // Max bytes of *payload* per fragment
        ReassemblyTimeout: 10 * time.Second,    // How long to wait for missing fragments
        CleanupInterval:   30 * time.Second,    // How often to check for timed-out buffers
    }
    debugEnabled := false // Enable verbose logging?

    udpfrag.Configure(myConfig, debugEnabled)

    // Now use DialUDPFrag or udplistener.NewUDPListener...
}

```

*   **`MaxFragmentSize`**: Max payload bytes per fragment. Header (10 bytes) is added. Defaults to `1400 - 10`.
*   **`ReassemblyTimeout`**: Timeout for incomplete messages. Defaults to 5s.
*   **`CleanupInterval`**: How often to clean up timed-out buffers. Defaults to 10s.
*   **`debugLog` (bool)**: Enables/disables verbose logging globally. Defaults to `false`.

### Client Side (`udpfrag.UDPClientConn`)

Use `DialUDPFrag` for client connections to a specific server address.

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/cjbrigato/udpfrag" // Use the client helper from this package
)

func main() {
	// Optional: Configure udpfrag first (see above)
	udpfrag.Configure(udpfrag.Config{}, false) // Use defaults, disable debug logs

	// Dial the remote UDP server
	conn, err := udpfrag.DialUDPFrag("udp", "127.0.0.1:8080", nil) // nil logger uses default
	if err != nil {
		log.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	log.Printf("Connected from %s to %s", conn.LocalAddr(), conn.RemoteAddr())

	message := []byte("This is a potentially large message that might be fragmented.")
	// Add more data to message to ensure fragmentation if needed...

	// Write - fragmentation happens automatically
	_, err = conn.Write(message)
	if err != nil {
		log.Fatalf("Failed to write: %v", err)
	}
	log.Printf("Wrote message.")

	// Read - reassembly happens automatically
	readBuffer := make([]byte, udpfrag.DefaultMaxFragmentSize*2) // Adjust buffer size
	n, err := conn.Read(readBuffer)
	if err != nil {
		log.Fatalf("Failed to read: %v", err)
	}
	log.Printf("Read %d bytes: %s\n", n, string(readBuffer[:n]))
}
```

### Server Side (`udplistener`)

Use the separate `udplistener` package for a `net.Listener`-like experience. It handles multiple clients and reassembly automatically using `udpfrag`.

```go
package main

import (
	"io"
	"log"
	"net"
	"time"

	"github.com/cjbrigato/udpfrag"     // For configuration
	"github.com/cjbrigato/udplistener" // Use the listener helper package
)

// handleConnection processes a single "virtual" connection from a client.
func handleConnection(conn net.Conn) {
	remoteAddr := conn.RemoteAddr()
	log.Printf("Handling connection from %s", remoteAddr)
	defer log.Printf("Closing connection from %s", remoteAddr)
	defer conn.Close() // Crucial to clean up listener state

	// Example: Echo server
	buffer := make([]byte, 8192) // Buffer for reassembled messages
	for {
		// Set a read deadline for this specific client connection
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second)) // e.g., 60s idle timeout

		n, err := conn.Read(buffer)
		if err != nil {
			if err != io.EOF && err != net.ErrClosed { // Don't log expected closure errors as harshly
                // Check for timeout error specifically
                if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                    log.Printf("Client %s timed out.", remoteAddr)
                } else {
				    log.Printf("Error reading from %s: %v", remoteAddr, err)
                }
			}
			break // Exit loop on error (including EOF/timeout/closed)
		}

		log.Printf("Received %d bytes from %s: %s", n, remoteAddr, string(buffer[:n]))

		// Write response back - fragmentation happens automatically if needed
		// Set a write deadline
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err = conn.Write(buffer[:n]) // Echo back
		if err != nil {
			log.Printf("Error writing to %s: %v", remoteAddr, err)
			break // Exit loop on write error
		}
	}
}

func main() {
	// Optional: Configure udpfrag first (applies globally)
	udpfrag.Configure(udpfrag.Config{ReassemblyTimeout: 15*time.Second}, true) // Custom timeout, enable debug logs

	// Create the UDP Listener using the udplistener package
	listener, err := udplistener.NewUDPListener(":8080", nil) // nil logger uses default
	if err != nil {
		log.Fatalf("Failed to create UDP listener: %v", err)
	}
	defer listener.Close() // Ensure listener stops cleanly

	log.Printf("UDP server listening on %s", listener.Addr())

	// Accept loop
	for {
		// Accept waits for the *first fully reassembled message* from a new client
		// and returns a net.Conn representing that client connection.
		conn, err := listener.Accept()
		if err != nil {
			// Check if the error is due to the listener being closed.
			if err == net.ErrClosed {
				log.Println("Listener closed, exiting accept loop.")
				break // Exit loop cleanly
			}
			log.Printf("Error accepting connection: %v", err)
			continue // Keep listening
		}

		// Handle each connection concurrently
		go handleConnection(conn)
	}
}
```

## Logging

Both `udpfrag` and `udplistener` use `udpfrag`'s conditional logger (`CondLogger`). Debug logging (globally enabled/disabled via `udpfrag.Configure`) prints detailed information about fragmentation, reassembly, timeouts, and connection states to `os.Stderr` by default.

You can provide your own `*udpfrag.CondLogger` instance to `udpfrag.DialUDPFrag` or `udplistener.NewUDPListener` if you need more control over log output (e.g., directing logs to a file or integrating with a different logging framework).

## Important Considerations

*   **UDP is Unreliable:** These packages solve message *size* limitations but **do not** add reliability (guaranteed delivery, strict ordering between messages). UDP packets (and thus entire reassembled messages) can still be lost, duplicated, or arrive out of order relative to other messages. Implement application-level checks (sequence numbers, ACKs, retries) if needed, or use TCP.
*   **No Built-in Integrity Checks:** No checksums beyond the standard UDP checksum are added. Add your own payload integrity checks (e.g., CRC32, hash) before sending and verify after receiving if data corruption is a concern.
*   **Resource Usage:** Reassembling messages requires temporary memory buffers. The `udplistener` manages connections per remote client. Configure `ReassemblyTimeout` appropriately via `udpfrag.Configure` to prevent stale state buildup. Ensure `conn.Close()` is called in server handlers to release resources associated with a client in the `udplistener`.
*   **Connection Model:**
    *   `udpfrag.UDPClientConn`: Represents a connection *to* a single remote server (uses `net.DialUDP`).
    *   `udplistener`: Listens for incoming packets and creates a virtual `net.Conn` (`UDPpseudoConn`) *per unique remote client address* once the first complete message is received from that client.

## Contributing

Contributions (bug reports, feature requests, pull requests) to `udpfrag` or `udplistener` are welcome! Please open an issue to discuss significant changes.

## License

Distributed under the MIT license. See `LICENSE` file for details.