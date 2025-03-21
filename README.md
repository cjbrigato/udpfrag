
# udpfrag - UDP Fragmentation and Reassembly Package for Go

[![GoDoc](https://godoc.org/github.com/cjbrigato/udpfrag?status.svg)](https://godoc.org/github.com/cjbrigato/udpfrag)

**udpfrag** is a Go package providing application-level UDP fragmentation and reassembly. It allows you to send UDP messages larger than the network MTU by automatically splitting them into fragments on the sender side and reassembling them back into the original message on the receiver side.

This package is designed to be production-ready, offering configurability, logging, and error handling for robust UDP communication in Go applications.

## Features

*   **Automatic Fragmentation:**  Splits large messages into UDP fragments smaller than the configured `MaxFragmentSize` before sending.
*   **Reliable Reassembly:** Reassembles incoming UDP fragments back into the original message based on message IDs and fragment numbers.
*   **Configurable Parameters:**
    *   `MaxFragmentSize`:  Adjust the maximum payload size of UDP fragments.
    *   `ReassemblyTimeout`:  Set the timeout duration for reassembly, discarding incomplete messages after timeout.
    *   `CleanupInterval`:  Configure the interval for periodic cleanup of timed-out reassembly buffers.
    *   Customizable Logger: Use your own `log.Logger` instance for package logging.
*   **Efficient Buffer Management:** Utilizes buffer pooling to minimize memory allocations and improve performance.
*   **Error Handling:** Provides a custom `ReassemblyError` type for specific reassembly-related issues.
*   **Production Ready:** Designed for integration into real-world applications with focus on robustness and configurability.

## Installation

To install the `udpfrag` package, use `go get`:

```bash
go get github.com/cjbrigato/udpfrag  
```

Then you can import it in your Go code:

```go
import "github.com/cjbrigato/udpfrag"
```

## Usage

### Configuration

You can configure the `udpfrag` package using the `udpfrag.Config` struct and the `udpfrag.Configure()` function.  If you don't configure it, default values will be used.

```go
package main

import (
	"log"
	"time"
	"github.com/cjbrigato/udpfrag"
)

func main() {
	config := udpfrag.Config{
		MaxFragmentSize:  1300,                      // Example: Custom MaxFragmentSize
		ReassemblyTimeout: 10 * time.Second,          // Example: Custom reassembly timeout
		CleanupInterval:   30 * time.Second,         // Example: Custom cleanup interval
		Logger:           log.New(log.Writer(), "myapp: ", log.LstdFlags), // Example: Custom logger
	}
	udpfrag.Configure(config)

    // ... rest of your application code ...
}
```

**Configuration Options:**

*   **`MaxFragmentSize int`**:  The maximum size (in bytes) of the data payload within a UDP fragment, excluding the `FragmentHeader`.  Defaults to `1400 - FragmentHeaderSize`.  Should be less than the network MTU minus UDP and IP header sizes.
*   **`ReassemblyTimeout time.Duration`**: The duration after which the reassembly process for a message is considered failed and the partially received fragments are discarded. Defaults to `5 * time.Second`.
*   **`CleanupInterval time.Duration`**: The interval at which the package periodically checks for and cleans up timed-out reassembly buffers. Defaults to `10 * time.Second`.
*   **`Logger *log.Logger`**:  A custom `log.Logger` instance to use for logging within the `udpfrag` package. If `nil`, a default logger will be used.

### Server-Side Usage (Receiving and Reassembling)

On the server side, you need to use `udpfrag.HandleUDPConn()` to listen for UDP packets, reassemble fragmented messages, and process the reassembled messages using a message handler function.

```go
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"github.com/cjbrigato/udpfrag" // 
)

func main() {
	udpAddr, err := net.ResolveUDPAddr("udp", ":20001")
	if err != nil {
		log.Fatalf("Error resolving UDP address: %v", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("Error listening on UDP: %v", err)
	}
	defer conn.Close()

	log.Println("UDP server listening on", udpAddr)

	// Set up signal handling for clean shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Message handler function (replace with your application logic)
	messageHandler := func(addr *net.UDPAddr, message []byte) error {
		log.Printf("Received reassembled message from %s: %d bytes", addr.String(), len(message))
		// Process your message here...
		return nil
	}

	// Start handling UDP connections in a goroutine
	go func() {
		udpfrag.HandleUDPConn(conn, messageHandler)
	}()

	// Wait for a shutdown signal
	<-sigChan
	log.Println("Shutting down server...")
	// Perform any cleanup here if needed before exiting
	log.Println("Server shutdown complete.")
}
```

**Important:**  Replace the example `messageHandler` with your actual message processing logic.

### Client-Side Usage (Fragmentation and Sending)

On the client side, use `udpfrag.FragmentData()` to fragment your data before sending it over UDP.

```go
package main

import (
	"log"
	"net"
	"time"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"github.com/cjbrigato/udpfrag" // Replace with your module path
)

func main() {
	serverAddr, err := net.ResolveUDPAddr("udp", "localhost:20001")
	if err != nil {
		log.Fatalf("Error resolving server address: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		log.Fatalf("Error dialing UDP: %v", err)
	}
	defer conn.Close()

	messagePayload := generateLargeMessage(2000) // Example: Generate a large message (replace with your data)
	checksum := crc32.ChecksumIEEE(messagePayload)

	fullMessage := new(bytes.Buffer)
	binary.Write(fullMessage, binary.BigEndian, checksum) // Prepend checksum
	fullMessage.Write(messagePayload)

	fragments, err := udpfrag.FragmentData(fullMessage.Bytes())
	if err != nil {
		log.Fatalf("Error fragmenting data: %v", err)
	}

	log.Println("Sending fragmented message...")
	for _, fragment := range fragments {
		_, err := conn.Write(fragment)
		if err != nil {
			log.Println("Error sending fragment:", err)
		}
		time.Sleep(10 * time.Millisecond) // Simulate network rate limiting
	}
	log.Println("Fragmented message sent.")
}


func generateLargeMessage(size int) []byte { // Example utility function
	data := make([]byte, size)
	// ... (fill data with content as needed) ...
	return data
}
```

### Example Message Handler

The `udpfrag.HandleUDPConn()` function requires a message handler function with the following signature:

```go
type MessageHandler func(addr *net.UDPAddr, message []byte) error
```

This function is called by `HandleUDPConn` whenever a complete message is reassembled.  You are responsible for implementing your application-specific logic within this handler, such as:

*   Decoding the message payload.
*   Verifying message integrity (e.g., checksum verification like in `udpfrag.ExampleMessageHandler`).
*   Processing the received data.
*   Potentially sending a response.

The handler function should return `nil` if the message was processed successfully, or an `error` if an error occurred during processing. Errors returned by the message handler will be logged by `udpfrag.HandleUDPConn`.

## Error Handling

The `udpfrag` package defines a custom error type `udpfrag.ReassemblyError`. This error type is returned by `udpfrag.ReassembleData()` and `udpfrag.assembleMessage()` in cases of reassembly failures, such as timeouts or invalid message IDs. You can check for this error type to handle reassembly-specific errors in your application.

```go
reassembledData, err := udpfrag.ReassembleData(packet)
if err != nil {
    if errors.As(err, &udpfrag.ReassemblyError{}) {
        log.Printf("Reassembly error: %v", err)
        // Handle reassembly error specifically (e.g., request retransmission, discard message)
    } else if err != nil {
        log.Printf("Other error during reassembly: %v", err)
        // Handle other errors
    }
}
```

## Performance Considerations

The `udpfrag` package is designed with performance in mind and includes optimizations like:

*   **Buffer Pooling:**  Reuses buffers for UDP reads and fragment headers to reduce memory allocations and garbage collection overhead.
*   **Efficient Assembly:**  Assembles messages directly into pre-allocated byte slices to minimize data copying.

For optimal performance in high-throughput scenarios, consider profiling your application with Go's `pprof` tool to identify any potential bottlenecks and fine-tune configurations like `MaxFragmentSize` and buffer sizes as needed.

## License

This package is licensed under the [MIT License](LICENSE).
