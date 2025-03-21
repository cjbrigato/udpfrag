package udpfrag

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

// Config structure to hold configuration parameters for the fragmentation/reassembly package.
type Config struct {
	MaxFragmentSize  int           // Maximum size of data payload in a fragment (after header)
	ReassemblyTimeout time.Duration // Timeout after which reassembly of a message is abandoned
	CleanupInterval   time.Duration // Interval for periodic cleanup of timed-out reassembly buffers
	Logger           *log.Logger   // Logger to use for logging messages (can be nil for default logger)
}

// Default configuration values.
const (
	DefaultMaxFragmentSize  = 1400 - FragmentHeaderSize // MTU typique Ethernet - En-tête UDP - En-tête Fragmentation
	DefaultReassemblyTimeout = 5 * time.Second
	DefaultCleanupInterval   = 10 * time.Second
	MaxUDPSize              = 65507 // Maximum standard UDP packet size (IPv4)
	FragmentHeaderSize      = 10    // Size in bytes of the fragmentation header
)

// FragmentHeader represents the header added to each UDP fragment.
type FragmentHeader struct {
	MessageID      uint32 // Unique identifier of the fragmented message
	FragmentNumber uint16 // Fragment number (starts at 0)
	TotalFragments uint16 // Total number of fragments for this message
	Flags          uint16 // Flags (e.g., last fragment)
}

// Flags for FragmentHeader.
const (
	FlagLastFragment uint16 = 1 << 0 // Flag bit to indicate the last fragment
)

// fragmentedMessage represents a message being reassembled.
type fragmentedMessage struct {
	fragmentsReceived [][]byte
	lastFragmentTime  time.Time
	totalFragments    uint16
}

var (
	reassemblyBuffers     = make(map[uint32]*fragmentedMessage)
	reassemblyBuffersMutex sync.Mutex
	udpReadBufferPool = sync.Pool{ // Pool to reuse UDP read buffers
		New: func() interface{} {
			return make([]byte, MaxUDPSize)
		},
	}
	fragmentHeaderBufferPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	globalConfig Config // Global configuration for the package
	defaultLogger  = log.New(log.Writer(), "udpfrag: ", log.LstdFlags) // Default logger if none provided
)

// ReassemblyError is a custom error type for reassembly related errors.
type ReassemblyError struct {
	Message string
}

func (e *ReassemblyError) Error() string {
	return fmt.Sprintf("reassembly error: %s", e.Message)
}

// ensureLogger returns the configured logger or the default logger if none is configured.
func ensureLogger() *log.Logger {
	if globalConfig.Logger != nil {
		return globalConfig.Logger
	}
	return defaultLogger
}

// InitializeDefaultConfig initializes the global configuration with default values.
func InitializeDefaultConfig() {
	globalConfig = Config{
		MaxFragmentSize:  DefaultMaxFragmentSize,
		ReassemblyTimeout: DefaultReassemblyTimeout,
		CleanupInterval:   DefaultCleanupInterval,
		Logger:           nil, // Use default logger
	}
}

// Configure allows setting up the package with a custom configuration.
func Configure(config Config) {
	globalConfig = config
	if globalConfig.MaxFragmentSize <= FragmentHeaderSize || globalConfig.MaxFragmentSize > MaxUDPSize-FragmentHeaderSize {
		ensureLogger().Printf("Warning: MaxFragmentSize is invalid, using default value: %d", DefaultMaxFragmentSize)
		globalConfig.MaxFragmentSize = DefaultMaxFragmentSize
	}
	if globalConfig.ReassemblyTimeout <= 0 {
		ensureLogger().Printf("Warning: ReassemblyTimeout is invalid, using default value: %s", DefaultReassemblyTimeout)
		globalConfig.ReassemblyTimeout = DefaultReassemblyTimeout
	}
	if globalConfig.CleanupInterval <= 0 {
		ensureLogger().Printf("Warning: CleanupInterval is invalid, using default value: %s", DefaultCleanupInterval)
		globalConfig.CleanupInterval = DefaultCleanupInterval
	}
}

func init() {
	InitializeDefaultConfig() // Initialize with default config at package load
	go cleanupReassemblyBuffers() // Start the cleanup goroutine
}


// generateMessageID generates a unique message ID randomly.
func generateMessageID() uint32 {
	return rand.Uint32()
}

// FragmentData divides the data into UDP fragments based on the configured MaxFragmentSize.
// It returns a slice of UDP packets (including headers) ready to be sent.
func FragmentData(data []byte) ([][]byte, error) {
	messageID := generateMessageID()
	maxFragmentPayloadSize := globalConfig.MaxFragmentSize
	numFragments := (len(data) + maxFragmentPayloadSize - 1) / maxFragmentPayloadSize
	if numFragments > 65535 {
		return nil, fmt.Errorf("message too large to be fragmented into less than 65536 fragments") // Should be ReassemblyError?
	}
	fragments := make([][]byte, 0, numFragments)

	for i := 0; i < numFragments; i++ {
		start := i * maxFragmentPayloadSize
		end := start + maxFragmentPayloadSize
		if end > len(data) {
			end = len(data)
		}
		fragmentData := data[start:end]

		header := FragmentHeader{
			MessageID:      messageID,
			FragmentNumber: uint16(i),
			TotalFragments: uint16(numFragments),
		}
		if i == numFragments-1 {
			header.Flags |= FlagLastFragment
		}

		headerBuf := fragmentHeaderBufferPool.Get().(*bytes.Buffer)
		if headerBuf == nil { // Safety check, should not happen with sync.Pool
			headerBuf = new(bytes.Buffer)
		}
		headerBuf.Reset() // Reset the buffer before reuse
		defer fragmentHeaderBufferPool.Put(headerBuf) // Return to pool after use

		if err := binary.Write(headerBuf, binary.BigEndian, header); err != nil {
			return nil, fmt.Errorf("binary.Write header failed: %w", err)
		}

		packet := append(headerBuf.Bytes(), fragmentData...)
		fragments = append(fragments, packet)
	}

	return fragments, nil
}

// ReassembleData attempts to reassemble UDP fragments into the original data.
// It returns the reassembled data if all fragments are received within the timeout, or nil and an error otherwise.
// If the packet is not a fragment (too small to contain a header), it returns nil, nil.
func ReassembleData(packet []byte) ([]byte, error) {
	if len(packet) < FragmentHeaderSize {
		return nil, nil // Not a fragment, or too small to be valid, ignore and return nil, nil
	}

	headerBuf := bytes.NewReader(packet[:FragmentHeaderSize])
	var header FragmentHeader
	if err := binary.Read(headerBuf, binary.BigEndian, &header); err != nil {
		return nil, fmt.Errorf("binary.Read header failed: %w", err)
	}
	fragmentPayload := packet[FragmentHeaderSize:]

	reassemblyBuffersMutex.Lock()
	defer reassemblyBuffersMutex.Unlock()

	msg, ok := reassemblyBuffers[header.MessageID]
	if !ok {
		msg = &fragmentedMessage{
			fragmentsReceived: make([][]byte, header.TotalFragments),
			lastFragmentTime:  time.Now(),
			totalFragments:    header.TotalFragments,
		}
		reassemblyBuffers[header.MessageID] = msg
	}

	if int(header.FragmentNumber) >= len(msg.fragmentsReceived) {
		return nil, fmt.Errorf("invalid fragment number: %d, total expected: %d", header.FragmentNumber, len(msg.fragmentsReceived))
	}
	msg.fragmentsReceived[header.FragmentNumber] = fragmentPayload
	msg.lastFragmentTime = time.Now()

	// Check if all fragments have arrived and timeout has not been exceeded
	if isComplete(msg) {
		reassembledData, err := assembleMessage(header.MessageID)
		if err != nil {
			return nil, err
		}
		delete(reassemblyBuffers, header.MessageID) // Clean up after reassembly
		return reassembledData, nil
	}

	return nil, nil // Reassembly incomplete, wait for more fragments
}

// isComplete checks if all fragments of a message have been received or if reassembly timeout is reached.
func isComplete(msg *fragmentedMessage) bool {
	if time.Since(msg.lastFragmentTime) > globalConfig.ReassemblyTimeout {
		return true // Consider complete (timeout) for cleanup, even if incomplete
	}
	for _, frag := range msg.fragmentsReceived {
		if frag == nil {
			return false // Missing fragment
		}
	}
	return true // All fragments received
}

// assembleMessage assembles the fragments into a complete message.
func assembleMessage(messageID uint32) ([]byte, error) {
	reassemblyBuffersMutex.Lock()
	msg, ok := reassemblyBuffers[messageID]
	delete(reassemblyBuffers, messageID) // Remove immediately to prevent double assembly
	reassemblyBuffersMutex.Unlock()

	if !ok {
		return nil, &ReassemblyError{Message: "message ID not found in reassembly buffers"}
	}

	if !isComplete(msg) && time.Since(msg.lastFragmentTime) <= globalConfig.ReassemblyTimeout {
		return nil, &ReassemblyError{Message: "reassembly incomplete, timeout not reached (should not happen if isComplete is correct)"}
	}

	totalSize := 0
	for _, frag := range msg.fragmentsReceived {
		if frag != nil {
			totalSize += len(frag)
		}
	}

	reassembledData := make([]byte, totalSize) // Pre-allocate slice of total size
	offset := 0
	for _, frag := range msg.fragmentsReceived {
		if frag != nil {
			copy(reassembledData[offset:], frag) // Direct copy with copy
			offset += len(frag)
		}
	}

	if len(reassembledData) == 0 && msg.totalFragments > 0 {
		return nil, &ReassemblyError{Message: "reassembly timeout, message incomplete or lost"}
	}

	return reassembledData, nil
}

// HandleUDPConn manages the UDP connection and reassembly process.
// It reads UDP packets from the connection and attempts to reassemble fragmented messages.
// For each reassembled message, it calls the messageHandler function.
func HandleUDPConn(conn *net.UDPConn, messageHandler func(addr *net.UDPAddr, message []byte) error) {
	defer conn.Close()

	for {
		buffer := udpReadBufferPool.Get().([]byte)
		if buffer == nil { // Safety check, should not happen with sync.Pool
			buffer = make([]byte, MaxUDPSize)
		}
		n, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			ensureLogger().Printf("UDP read error: %v", err)
			udpReadBufferPool.Put(buffer)
			return // Exit handler on read error
		}

		reassembledData, err := ReassembleData(buffer[:n])
		if err != nil {
			ensureLogger().Printf("Reassembly error: %v", err)
			udpReadBufferPool.Put(buffer)
			continue // Continue listening even on reassembly error for this packet
		}
		udpReadBufferPool.Put(buffer)

		if reassembledData != nil {
			// Message reassembled successfully
			if messageHandler != nil {
				if err := messageHandler(addr, reassembledData); err != nil {
					ensureLogger().Printf("MessageHandler error for message from %s: %v", addr.String(), err)
				}
			} else {
				ensureLogger().Println("Reassembled message received, but no message handler provided.")
			}
		} else {
			// Reassembly incomplete, waiting for more fragments
			ensureLogger().Printf("Fragment received from %s, reassembly in progress...", addr.String())
		}
	}
}

// cleanupReassemblyBuffers is a goroutine that periodically cleans up timed-out reassembly buffers.
func cleanupReassemblyBuffers() {
	ticker := time.NewTicker(globalConfig.CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		reassemblyBuffersMutex.Lock()
		for msgID, msg := range reassemblyBuffers {
			if time.Since(msg.lastFragmentTime) > globalConfig.ReassemblyTimeout && !isComplete(msg) { // Check timeout and incompleteness
				ensureLogger().Printf("Removing incomplete message %d (timeout)", msgID)
				delete(reassemblyBuffers, msgID)
			}
		}
		reassemblyBuffersMutex.Unlock()
	}
}


// ExampleMessageHandler is a sample message handler function that verifies a checksum and prints the message.
// This is meant for demonstration purposes and can be replaced with application-specific logic.
func ExampleMessageHandler(addr *net.UDPAddr, message []byte) error {
	if len(message) < 4 {
		return errors.New("message too short to contain checksum")
	}
	checksum := binary.BigEndian.Uint32(message[:4]) // Assume first 4 bytes are checksum
	dataPayload := message[4:]

	calculatedChecksum := crc32.ChecksumIEEE(dataPayload)
	if calculatedChecksum != checksum {
		ensureLogger().Printf("Invalid checksum received from %s, data corrupted!", addr.String())
		return errors.New("invalid checksum")
	}

	fmt.Printf("Reassembled message received from %s: %s\n", addr.String(), string(dataPayload)) // Using fmt.Printf for example output
	return nil
}
