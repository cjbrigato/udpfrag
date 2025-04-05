package udpfrag

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// UDPClientConn provides a net.Conn interface over a UDP connection,
// handling fragmentation and reassembly automatically.
type UDPClientConn struct {
	conn       *net.UDPConn      // Underlying "connected" UDP socket
	remoteAddr net.Addr          // Cached remote address
	localAddr  net.Addr          // Cached local address
	incoming   chan []byte       // Channel for reassembled messages for Read()
	readCtx    context.Context   // Context for Read deadlines
	readCancel context.CancelFunc
	wg         sync.WaitGroup    // To wait for readLoop
	closeOnce  sync.Once
	closed     chan struct{}     // Signals closure to readLoop
	logger     *CondLogger       // Conditional logger
}

// DialUDPFrag establishes a "connected" UDP socket to the remote address
// and returns a UDPClientConn that handles fragmentation/reassembly.
// If logger is nil, the package's default logger will be used.
func DialUDPFrag(network, address string, logger *CondLogger) (*UDPClientConn, error) {
	// Use provided logger or package default
	log := logger
	if log == nil {
		log = ensureLogger()
	}

	// Resolve remote address
	raddr, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		log.Printf("DialUDPFrag: Failed to resolve remote UDP address %s: %v", address, err)
		return nil, fmt.Errorf("failed to resolve remote UDP address %s: %w", address, err)
	}

	// Dial creates a "connected" UDP socket
	udpConn, err := net.DialUDP(network, nil, raddr) // nil local addr = OS chooses
	if err != nil {
		log.Printf("DialUDPFrag: Failed to dial UDP %s: %v", address, err)
		return nil, fmt.Errorf("failed to dial UDP %s: %w", address, err)
	}

	log.Printf("DialUDPFrag: Connected to %s from %s", udpConn.RemoteAddr(), udpConn.LocalAddr())

	readCtx, readCancel := context.WithCancel(context.Background())

	clientConn := &UDPClientConn{
		conn:       udpConn,
		remoteAddr: udpConn.RemoteAddr(), // Cache addresses
		localAddr:  udpConn.LocalAddr(),
		incoming:   make(chan []byte, 10), // Buffer for reassembled messages
		readCtx:    readCtx,
		readCancel: readCancel,
		closed:     make(chan struct{}),
		logger:     log,
	}

	// Start the background reader goroutine
	clientConn.wg.Add(1)
	go clientConn.readLoop()

	return clientConn, nil
}

// readLoop continuously reads from the underlying UDP connection,
// attempts reassembly, and sends completed messages to the incoming channel.
func (c *UDPClientConn) readLoop() {
	defer c.wg.Done()
	// Ensure incoming channel is closed on exit to unblock Read calls
	defer close(c.incoming)
	c.logger.Printf("ClientConn readLoop started for %s -> %s", c.localAddr, c.remoteAddr)
	defer c.logger.Printf("ClientConn readLoop finished for %s -> %s", c.localAddr, c.remoteAddr)

	readBufPool := GetUDPReadBufferPool() // Use the shared buffer pool

	for {
		// Check for closure before reading
		select {
		case <-c.closed:
			return // Exit loop if connection is closed
		default:
			// Proceed to read
		}

		buffer := readBufPool.Get().([]byte)
		// Set a read deadline on the underlying socket. This helps ensure Read() doesn't block indefinitely
		// if the connection is closed externally or encounters issues. Use a reasonably long default?
		// Or rely on the context deadline set via SetReadDeadline? Relying on context is cleaner.
		// We'll primarily rely on context, but a short socket deadline prevents indefinite block.
		_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)) // Example: 5 sec socket timeout

		n, err := c.conn.Read(buffer) // Read should only return packets from the connected peer

		if err != nil {
			readBufPool.Put(buffer) // Return buffer on error
			select {
			case <-c.closed:
				// This error is expected if Close() was called
				c.logger.Printf("ClientConn Read error after Close: %v", err)
			default:
				// Unexpected error
				c.logger.Printf("ClientConn Read error: %v", err)
				// Trigger Close() to ensure proper cleanup and signal Read waiters
				c.Close()
			}
			return // Exit loop
		}

		// We received a packet, attempt reassembly
		// Make a copy as buffer will be reused
		packetCopy := make([]byte, n)
		copy(packetCopy, buffer[:n])
		readBufPool.Put(buffer) // Return buffer

		reassembledData, fragErr := ReassembleData(packetCopy)

		if fragErr != nil {
			c.logger.Printf("ClientConn reassembly error: %v", fragErr)
			continue // Ignore this packet, wait for more
		}

		if reassembledData == nil {
			// Fragment received, reassembly in progress (handled internally by ReassembleData)
			// Log only if needed, can be noisy: c.logger.Printf("ClientConn fragment received...")
			continue
		}

		// Reassembly complete! Send to the Read() method via the channel.
		// Use a select with context to handle potential Read timeouts while sending.
		select {
		case c.incoming <- reassembledData:
			c.logger.Printf("ClientConn reassembled message (%d bytes) queued for application Read()", len(reassembledData))
		case <-c.closed:
			c.logger.Println("ClientConn closed while trying to queue reassembled message.")
			return // Exit if closed
			// No default case needed; we want to block until Read consumes or closed.
			// If the 'incoming' channel buffer fills up, this indicates the application
			// isn't calling Read() fast enough. Blocking here applies backpressure.
		}
	}
}

// Read reads a fully reassembled message from the connection.
func (c *UDPClientConn) Read(b []byte) (n int, err error) {
	// Check if context is already done (deadline expired/canceled)
	select {
	case <-c.readCtx.Done():
		return 0, c.readCtx.Err()
	default:
	}

	// Wait for message from incoming channel or context cancellation/deadline
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-c.readCtx.Done():
		return 0, c.readCtx.Err()
	case data, ok := <-c.incoming:
		if !ok {
			// Channel closed, likely due to Close() or readLoop error
			return 0, net.ErrClosed
		}
		if len(data) > len(b) {
			copy(b, data[:len(b)])
			// Return error on truncation for clarity
			return len(b), fmt.Errorf("buffer too small, message truncated (need %d, have %d)", len(data), len(b))
		}
		copy(b, data)
		return len(data), nil
	}
}

// Write fragments data if necessary and sends it to the connected remote address.
func (c *UDPClientConn) Write(b []byte) (n int, err error) {
	// Check if closed first outside select for quick exit
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}

	// Fragment the data
	fragments, err := FragmentData(b) // Use the package's fragmentation logic
	if err != nil {
		return 0, fmt.Errorf("clientconn failed to fragment data: %w", err)
	}

	// Send each fragment using the underlying connected socket's Write
	bytesWritten := 0 // Track raw bytes for logging if needed
	c.logger.Printf("ClientConn writing %d app bytes as %d fragments", len(b), len(fragments))
	for i, fragment := range fragments {
		// We should respect the WriteDeadline set via SetWriteDeadline
		// The deadline is set on the underlying c.conn
		nn, writeErr := c.conn.Write(fragment) // Write on connected UDP socket
		if writeErr != nil {
			c.logger.Printf("ClientConn failed to write fragment %d: %v", i, writeErr)
			// Return 0 and error, indicating total failure for simplicity,
			// as partial UDP writes are not easily handled/meaningful.
			return 0, fmt.Errorf("failed to write fragment %d: %w", i, writeErr)
		}
		if nn != len(fragment) {
			// Short writes are unusual for UDP but check anyway
			c.logger.Printf("ClientConn short write on fragment %d: wrote %d, expected %d", i, nn, len(fragment))
			return 0, fmt.Errorf("short write on fragment %d: wrote %d, expected %d", i, nn, len(fragment))
		}
		bytesWritten += nn
	}
	c.logger.Printf("ClientConn successfully wrote %d fragments (%d raw bytes)", len(fragments), bytesWritten)

	// If all fragments sent successfully, return original length.
	return len(b), nil

}

// Close closes the connection and stops the background reader.
func (c *UDPClientConn) Close() error {
	c.closeOnce.Do(func() {
		c.logger.Printf("ClientConn closing connection %s -> %s", c.localAddr, c.remoteAddr)
		close(c.closed)      // Signal readLoop to stop
		c.readCancel()       // Cancel deadline context for Read()
		c.conn.Close()       // Close underlying UDP socket (unblocks Read in readLoop)
		c.wg.Wait()          // Wait for readLoop goroutine to finish
		// 'incoming' channel is closed by readLoop defer statement
		c.logger.Printf("ClientConn closed.")
	})
	// net.Conn interface expects error. c.conn.Close() error often nil for UDP.
	// We could capture it, but nil is usually fine here.
	return nil
}

// LocalAddr returns the local network address.
func (c *UDPClientConn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr returns the remote network address.
func (c *UDPClientConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline sets the read and write deadlines associated with the connection.
func (c *UDPClientConn) SetDeadline(t time.Time) error {
	// Set read deadline via context cancellation
	if err := c.SetReadDeadline(t); err != nil {
		// This should ideally not fail, but propagate if it does
		return fmt.Errorf("failed to set read deadline: %w", err)
	}
	// Set write deadline on underlying socket
	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls.
// A zero value for t means Read will not time out.
func (c *UDPClientConn) SetReadDeadline(t time.Time) error {
	c.readCancel() // Cancel previous context first
	if t.IsZero() {
		// Create a new non-cancelable context if deadline is zeroed
		c.readCtx, c.readCancel = context.WithCancel(context.Background())
	} else {
		// Create a new context with the specified deadline
		c.readCtx, c.readCancel = context.WithDeadline(context.Background(), t)
	}
	c.logger.Printf("ClientConn read deadline set to: %v", t)
	return nil // Return nil on success
}

// SetWriteDeadline sets the deadline for future Write calls.
// A zero value for t means Write will not time out.
func (c *UDPClientConn) SetWriteDeadline(t time.Time) error {
	// Apply to the underlying UDP connection
	c.logger.Printf("ClientConn write deadline set to: %v", t)
	return c.conn.SetWriteDeadline(t)
}