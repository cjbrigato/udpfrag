package udpfrag

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"
)

// Config structure to hold configuration parameters for the fragmentation/reassembly package.
type Config struct {
	MaxFragmentSize   int           // Maximum size of data payload in a fragment (after header)
	ReassemblyTimeout time.Duration // Timeout after which reassembly of a message is abandoned
	CleanupInterval   time.Duration // Interval for periodic cleanup of timed-out reassembly buffers
	// Logger is now handled by CondLogger at the package level, not directly in Config
}

// Default configuration values.
const (
	DefaultMaxFragmentSize  = 1400 - FragmentHeaderSize // Typical Ethernet MTU - UDP Header - Frag Header
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
	// Consider adding total size expected if needed for pre-allocation, though calculable.
}

var (
	reassemblyBuffers      = make(map[uint32]*fragmentedMessage)
	reassemblyBuffersMutex sync.Mutex
	udpReadBufferPool      = sync.Pool{ // Pool to reuse UDP read buffers
		New: func() interface{} {
			// Allocate slightly larger than MaxUDPSize just in case, common practice
			// but ensure it matches expectations. Let's stick to MaxUDPSize for clarity.
			return make([]byte, MaxUDPSize)
		},
	}
	fragmentHeaderBufferPool = sync.Pool{ // Pool for header serialization buffers
		New: func() interface{} {
			// Preallocate close to header size for efficiency
			return bytes.NewBuffer(make([]byte, 0, FragmentHeaderSize))
		},
	}
	globalConfig        Config       // Global configuration for the package
	defaultCondLogger   *CondLogger  // Default conditional logger
	defaultLoggerOutput io.Writer = os.Stderr // Default output for logger
)

// ReassemblyError is a custom error type for reassembly related errors.
type ReassemblyError struct {
	Message string
}

func (e *ReassemblyError) Error() string {
	return fmt.Sprintf("reassembly error: %s", e.Message)
}

// CondLogger provides conditional logging based on a boolean flag.
type CondLogger struct {
	logger *log.Logger
	debug  bool
	mu     sync.Mutex // Protect access to logger/debug flag if needed concurrently
}

// NewCondLogger creates a new conditional logger.
// If logger is nil, it defaults to log.Default().
func NewCondLogger(out io.Writer, prefix string, flag int, debug bool) *CondLogger {
	w := out
	if w == nil {
		w = defaultLoggerOutput // Use package default output if nil
	}
	l := log.New(w, prefix, flag)
	return &CondLogger{logger: l, debug: debug}
}

// Printf logs only if debug logging is enabled.
func (cl *CondLogger) Printf(format string, v ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.debug {
		cl.logger.Printf(format, v...)
	}
}

// Println logs only if debug logging is enabled.
func (cl *CondLogger) Println(v ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.debug {
		cl.logger.Println(v...)
	}
}

// SetDebug enables or disables debug logging.
func (cl *CondLogger) SetDebug(debug bool) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.debug = debug
}

// Ensure default logger is initialized
func ensureLogger() *CondLogger {
	// Could add a sync.Once here if initialization needs protection
	if defaultCondLogger == nil {
		defaultCondLogger = NewCondLogger(defaultLoggerOutput, "udpfrag: ", log.LstdFlags|log.Lmicroseconds, false) // Debug off by default
	}
	return defaultCondLogger
}

// InitializeDefaultConfig initializes the global configuration with default values.
func InitializeDefaultConfig() {
	globalConfig = Config{
		MaxFragmentSize:   DefaultMaxFragmentSize,
		ReassemblyTimeout: DefaultReassemblyTimeout,
		CleanupInterval:   DefaultCleanupInterval,
	}
}

// Configure allows setting up the package with a custom configuration and debug logging.
func Configure(config Config, debugLog bool) {
	globalConfig = config
	if globalConfig.MaxFragmentSize <= 0 || globalConfig.MaxFragmentSize > MaxUDPSize-FragmentHeaderSize {
		ensureLogger().Printf("Warning: MaxFragmentSize (%d) is invalid or too large, using default value: %d", globalConfig.MaxFragmentSize, DefaultMaxFragmentSize)
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

	// Configure the default logger verbosity
	ensureLogger().SetDebug(debugLog)
	ensureLogger().Printf("udpfrag configured: MaxFragmentSize=%d, Timeout=%s, Cleanup=%s, DebugLog=%t",
		globalConfig.MaxFragmentSize, globalConfig.ReassemblyTimeout, globalConfig.CleanupInterval, debugLog)
}

func init() {
	// Seed random generator for message IDs
	// Use crypto/rand for better uniqueness if needed, but math/rand is simpler for non-security use
	rand.Seed(time.Now().UnixNano())
	InitializeDefaultConfig() // Initialize with default config at package load
	// Start the cleanup goroutine using the package logger
	go cleanupReassemblyBuffers(ensureLogger())
}

// GetUDPReadBufferPool returns the package's UDP read buffer pool.
// Exported function to allow other packages (like udplistener) to use it.
func GetUDPReadBufferPool() *sync.Pool {
	return &udpReadBufferPool
}

// generateMessageID generates a unique message ID randomly.
func generateMessageID() uint32 {
	// Consider using a counter + randomness or crypto/rand if collisions are a major concern
	return rand.Uint32()
}

// FragmentData divides the data into UDP fragments based on the configured MaxFragmentSize.
// It returns a slice of UDP packets (including headers) ready to be sent.
func FragmentData(data []byte) ([][]byte, error) {
	logger := ensureLogger()
	if len(data) == 0 {
		return nil, errors.New("cannot fragment empty data")
	}

	messageID := generateMessageID()
	maxFragmentPayloadSize := globalConfig.MaxFragmentSize
	if maxFragmentPayloadSize <= 0 { // Safety check
		maxFragmentPayloadSize = DefaultMaxFragmentSize
	}

	// Calculate number of fragments needed
	numFragments := (len(data) + maxFragmentPayloadSize - 1) / maxFragmentPayloadSize
	if numFragments == 0 && len(data) > 0 { // Should only happen if maxFragmentPayloadSize is huge
		numFragments = 1
	}

	if numFragments > 65535 {
		return nil, fmt.Errorf("message too large (%d bytes) to be fragmented into <= 65535 fragments with fragment size %d", len(data), maxFragmentPayloadSize)
	}
	if numFragments == 1 && len(data)+FragmentHeaderSize <= maxFragmentPayloadSize+FragmentHeaderSize {
		logger.Printf("Data size (%d) fits in single packet, no fragmentation needed technically, but sending as one fragment.", len(data))
		// Still proceed to send as one fragment for consistency, receiver expects header.
	}

	logger.Printf("Fragmenting message %d (%d bytes) into %d fragments (max payload %d bytes)", messageID, len(data), numFragments, maxFragmentPayloadSize)

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
			Flags:          0, // Reset flags initially
		}
		if i == numFragments-1 {
			header.Flags |= FlagLastFragment
		}

		headerBuf := fragmentHeaderBufferPool.Get().(*bytes.Buffer)
		// No need for nil check, sync.Pool's New guarantees non-nil
		headerBuf.Reset() // Reset for reuse

		// Write header to buffer
		err := binary.Write(headerBuf, binary.BigEndian, header)
		if err != nil {
			fragmentHeaderBufferPool.Put(headerBuf) // Return buffer on error
			return nil, fmt.Errorf("failed to write fragment header %d for message %d: %w", i, messageID, err)
		}

		// Create the final packet: header + payload
		// Allocate exact size needed
		packet := make([]byte, FragmentHeaderSize+len(fragmentData))
		copy(packet[:FragmentHeaderSize], headerBuf.Bytes())
		copy(packet[FragmentHeaderSize:], fragmentData)

		fragments = append(fragments, packet)
		fragmentHeaderBufferPool.Put(headerBuf) // Return buffer to pool
	}

	return fragments, nil
}

// ReassembleData attempts to reassemble UDP fragments into the original data.
// It returns the reassembled data if all fragments are received, or nil otherwise.
// It returns an error only for critical issues like invalid headers or buffer mismatches.
// If the packet is not a fragment (too small) or if reassembly is just incomplete, it returns (nil, nil).
func ReassembleData(packet []byte) ([]byte, error) {
	logger := ensureLogger()
	if len(packet) < FragmentHeaderSize {
		// Packet is too small to possibly contain our header. Treat as non-fragmented data?
		// For this package's purpose, we assume inputs are potentially fragments.
		// So, if too small, it's not *our* fragment. Return nil, nil.
		logger.Printf("Received packet too small (%d bytes) to be a fragment, ignoring.", len(packet))
		return nil, nil
	}

	headerBuf := bytes.NewReader(packet[:FragmentHeaderSize])
	var header FragmentHeader
	if err := binary.Read(headerBuf, binary.BigEndian, &header); err != nil {
		// Invalid header format
		return nil, fmt.Errorf("failed to read fragment header: %w", err)
	}
	fragmentPayload := packet[FragmentHeaderSize:]

	// Basic validation
	if header.TotalFragments == 0 {
		return nil, fmt.Errorf("invalid fragment header: TotalFragments is zero (ID %d)", header.MessageID)
	}
	if header.FragmentNumber >= header.TotalFragments {
		return nil, fmt.Errorf("invalid fragment number %d >= TotalFragments %d (ID %d)", header.FragmentNumber, header.TotalFragments, header.MessageID)
	}

	reassemblyBuffersMutex.Lock()
	defer reassemblyBuffersMutex.Unlock()

	msg, ok := reassemblyBuffers[header.MessageID]
	if !ok {
		// First fragment received for this message ID
		if header.TotalFragments > 65535 { // Redundant? NumFragments check in FragmentData
			return nil, fmt.Errorf("invalid TotalFragments %d (ID %d)", header.TotalFragments, header.MessageID)
		}
		logger.Printf("First fragment received for message %d (expecting %d total)", header.MessageID, header.TotalFragments)
		msg = &fragmentedMessage{
			fragmentsReceived: make([][]byte, header.TotalFragments), // Allocate slice for all fragments
			lastFragmentTime:  time.Now(),
			totalFragments:    header.TotalFragments,
		}
		reassemblyBuffers[header.MessageID] = msg
	} else {
		// Existing message buffer
		if header.TotalFragments != msg.totalFragments {
			// Mismatch in total fragments count within the same message ID - corruption likely
			logger.Printf("Error: TotalFragments mismatch for message %d (received %d, expected %d). Discarding buffer.", header.MessageID, header.TotalFragments, msg.totalFragments)
			delete(reassemblyBuffers, header.MessageID) // Discard inconsistent buffer
			// Treat this fragment as the start of a potentially new (but likely corrupt) message?
			// Safer to just return an error here indicating inconsistency.
			return nil, fmt.Errorf("total fragment count mismatch for message ID %d", header.MessageID)
		}
	}

	// Store the fragment payload if we haven't received this one yet
	if msg.fragmentsReceived[header.FragmentNumber] == nil {
		// Make a copy of the payload to store, as the original buffer might be reused
		payloadCopy := make([]byte, len(fragmentPayload))
		copy(payloadCopy, fragmentPayload)
		msg.fragmentsReceived[header.FragmentNumber] = payloadCopy
		logger.Printf("Stored fragment %d/%d for message %d (%d bytes)", header.FragmentNumber+1, header.TotalFragments, header.MessageID, len(payloadCopy))
	} else {
		// Duplicate fragment received, ignore it but update timestamp
		logger.Printf("Duplicate fragment %d/%d received for message %d, ignoring payload.", header.FragmentNumber+1, header.TotalFragments, header.MessageID)
	}
	msg.lastFragmentTime = time.Now() // Update timestamp on any activity

	// Check if reassembly is complete
	receivedCount := 0
	for _, frag := range msg.fragmentsReceived {
		if frag != nil {
			receivedCount++
		}
	}

	logger.Printf("Message %d status: %d/%d fragments received.", header.MessageID, receivedCount, msg.totalFragments)

	if receivedCount == int(msg.totalFragments) {
		// All fragments received! Attempt assembly.
		logger.Printf("All fragments received for message %d. Assembling...", header.MessageID)
		// We need to assemble outside the lock to avoid holding it during potentially large copies.
		// Remove from buffer *now* under lock to prevent double assembly.
		fragmentsToAssemble := msg.fragmentsReceived
		delete(reassemblyBuffers, header.MessageID) // Remove *before* unlocking

		// Unlock happens via defer reassemblyBuffersMutex.Unlock()

		// --- Assembly logic (outside lock) ---
		totalSize := 0
		for _, frag := range fragmentsToAssemble {
			// Should not be nil here, but check anyway
			if frag != nil {
				totalSize += len(frag)
			}
		}

		if totalSize > MaxUDPSize*int(header.TotalFragments) { // Sanity check total size
			return nil, fmt.Errorf("reassembled size %d exceeds theoretical maximum for message %d", totalSize, header.MessageID)
		}

		reassembledData := make([]byte, totalSize)
		offset := 0
		for i := 0; i < int(header.TotalFragments); i++ {
			frag := fragmentsToAssemble[i]
			if frag == nil {
				// This should not happen if receivedCount == totalFragments check was correct
				return nil, fmt.Errorf("internal error: missing fragment %d during final assembly of message %d", i, header.MessageID)
			}
			copy(reassembledData[offset:], frag)
			offset += len(frag)
			fragmentsToAssemble[i] = nil // Allow GC of fragment data
		}

		logger.Printf("Message %d assembled successfully (%d bytes).", header.MessageID, totalSize)
		return reassembledData, nil
		// --- End Assembly logic ---
	}

	// Reassembly incomplete, wait for more fragments
	return nil, nil
}

// cleanupReassemblyBuffers is a goroutine that periodically cleans up timed-out reassembly buffers.
func cleanupReassemblyBuffers(logger *CondLogger) {
	// Use the correct global config for interval, check if > 0
	interval := DefaultCleanupInterval
	if globalConfig.CleanupInterval > 0 {
		interval = globalConfig.CleanupInterval
	} else {
		logger.Printf("Warning: CleanupInterval is zero or negative, defaulting to %s", DefaultCleanupInterval)
	}
	if interval <= 0 { // Final safety check
		logger.Printf("Error: Invalid CleanupInterval (%s), cleanup disabled.", interval)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Printf("Cleanup goroutine started with interval %s", interval)

	for range ticker.C {
		reassemblyBuffersMutex.Lock()
		now := time.Now()
		// Use correct global config for timeout, check if > 0
		timeout := DefaultReassemblyTimeout
		if globalConfig.ReassemblyTimeout > 0 {
			timeout = globalConfig.ReassemblyTimeout
		} else {
			// Log warning if timeout is invalid, but still proceed with default? Or disable cleanup?
			// Let's use default if configured one is invalid.
			logger.Printf("Warning: ReassemblyTimeout is zero or negative, using default %s for cleanup check", DefaultReassemblyTimeout)
			timeout = DefaultReassemblyTimeout
		}

		removedCount := 0
		// Iterate safely over map
		for msgID, msg := range reassemblyBuffers {
			// Check if timeout has occurred since last fragment arrival
			if now.Sub(msg.lastFragmentTime) > timeout {
				// Log removal, check if it was actually incomplete
				receivedCount := 0
				for _, frag := range msg.fragmentsReceived {
					if frag != nil {
						receivedCount++
					}
				}
				logger.Printf("Cleanup: Removing timed-out message buffer for ID %d (%d/%d fragments received, last activity: %s)",
					msgID, receivedCount, msg.totalFragments, msg.lastFragmentTime.Format(time.RFC3339))
				delete(reassemblyBuffers, msgID)
				removedCount++
			}
		}
		if removedCount > 0 {
			logger.Printf("Cleanup: Removed %d timed-out message buffers.", removedCount)
		}
		reassemblyBuffersMutex.Unlock()
	}
	logger.Printf("Cleanup goroutine finished.") // Should ideally not happen unless program exits
}

// ExampleMessageHandler is a sample message handler function. REMOVE or keep for testing.
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