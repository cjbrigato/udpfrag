package udpfrag

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32" // Kept for ExampleMessageHandler, not used internally
	"io"
	"log"
	"math/rand"
	"net" // Kept for ExampleMessageHandler
	"os"
	"sync"
	"sync/atomic" // Added for atomic counter
	"time"
)

// Config structure to hold configuration parameters for the fragmentation/reassembly package.
type Config struct {
	MaxFragmentSize           int           // Maximum size of data payload in a fragment (after header)
	ReassemblyTimeout         time.Duration // Timeout after which reassembly of a message is abandoned
	CleanupInterval           time.Duration // Interval for periodic cleanup of timed-out reassembly buffers
	MaxConcurrentReassemblies int64         // Maximum number of messages being reassembled simultaneously across all shards (limits total map entries) - Changed to int64 for atomic
}

// Default configuration values.
const (
	DefaultMaxFragmentSize           = 1400 - FragmentHeaderSize // Typical Ethernet MTU - UDP Header - Frag Header
	DefaultReassemblyTimeout         = 5 * time.Second
	DefaultCleanupInterval           = 10 * time.Second
	DefaultMaxConcurrentReassemblies = 10000 // Default limit for concurrent reassemblies
	MaxUDPSize                       = 65507 // Maximum standard UDP packet size (IPv4)
	FragmentHeaderSize               = 10    // Size in bytes of the fragmentation header
	numShards                        = 64    // Number of shards for the reassembly buffer map - Power of 2 often good
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

// reassemblyShard holds one shard of the reassembly buffer map and its lock.
type reassemblyShard struct {
	buffers map[uint32]*fragmentedMessage
	mutex   sync.Mutex
}

var (
	// Sharded reassembly buffers to reduce mutex contention.
	shards [numShards]reassemblyShard
	// Atomic counter for the total number of messages being reassembled across all shards.
	totalReassemblyCount atomic.Int64

	udpReadBufferPool = sync.Pool{ // Pool to reuse UDP read buffers
		New: func() interface{} {
			return make([]byte, MaxUDPSize)
		},
	}
	fragmentHeaderBufferPool = sync.Pool{ // Pool for header serialization buffers
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, FragmentHeaderSize))
		},
	}
	globalConfig        Config                  // Global configuration for the package
	defaultCondLogger   *CondLogger             // Default conditional logger
	defaultLoggerOutput io.Writer   = os.Stderr // Default output for logger
)

// ReassemblyError is a custom error type for reassembly related errors.
type ReassemblyError struct {
	Message string
}

func (e *ReassemblyError) Error() string {
	return fmt.Sprintf("reassembly error: %s", e.Message)
}

// Specific error for exceeding the reassembly limit.
var ErrReassemblyLimitExceeded = &ReassemblyError{Message: "maximum concurrent reassemblies limit reached"}

// CondLogger provides conditional logging based on a boolean flag.
type CondLogger struct {
	logger *log.Logger
	debug  bool
	mu     sync.Mutex // Protect access to logger/debug flag if needed concurrently
}

// NewCondLogger creates a new conditional logger.
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
		MaxFragmentSize:           DefaultMaxFragmentSize,
		ReassemblyTimeout:         DefaultReassemblyTimeout,
		CleanupInterval:           DefaultCleanupInterval,
		MaxConcurrentReassemblies: DefaultMaxConcurrentReassemblies, // Initialize new default
	}
}

// Configure allows setting up the package with a custom configuration and debug logging.
func Configure(config Config, debugLog bool) {
	// Ensure MaxConcurrentReassemblies is positive
	if config.MaxConcurrentReassemblies <= 0 {
		ensureLogger().Printf("Warning: MaxConcurrentReassemblies (%d) is invalid, using default value: %d", config.MaxConcurrentReassemblies, DefaultMaxConcurrentReassemblies)
		config.MaxConcurrentReassemblies = DefaultMaxConcurrentReassemblies
	}
	globalConfig = config
	logger := ensureLogger() // Use local var for clarity

	// Validate other configuration values
	if globalConfig.MaxFragmentSize <= 0 || globalConfig.MaxFragmentSize > MaxUDPSize-FragmentHeaderSize {
		logger.Printf("Warning: MaxFragmentSize (%d) is invalid or too large, using default value: %d", globalConfig.MaxFragmentSize, DefaultMaxFragmentSize)
		globalConfig.MaxFragmentSize = DefaultMaxFragmentSize
	}
	if globalConfig.ReassemblyTimeout <= 0 {
		logger.Printf("Warning: ReassemblyTimeout is invalid, using default value: %s", DefaultReassemblyTimeout)
		globalConfig.ReassemblyTimeout = DefaultReassemblyTimeout
	}
	if globalConfig.CleanupInterval <= 0 {
		logger.Printf("Warning: CleanupInterval is invalid, using default value: %s", DefaultCleanupInterval)
		globalConfig.CleanupInterval = DefaultCleanupInterval
	}
	// Note: MaxConcurrentReassemblies already validated above

	// Configure the default logger verbosity
	logger.SetDebug(debugLog)
	logger.Printf("udpfrag configured: MaxFragmentSize=%d, Timeout=%s, Cleanup=%s, MaxConcurrent=%d, DebugLog=%t",
		globalConfig.MaxFragmentSize, globalConfig.ReassemblyTimeout, globalConfig.CleanupInterval, globalConfig.MaxConcurrentReassemblies, debugLog)
}

func init() {
	// Seed random generator for message IDs
	rand.Seed(time.Now().UnixNano())

	// Initialize shards
	for i := 0; i < numShards; i++ {
		shards[i] = reassemblyShard{
			buffers: make(map[uint32]*fragmentedMessage),
			// Mutex is zero-value initialized correctly
		}
	}

	InitializeDefaultConfig() // Initialize with default config at package load
	// Start the cleanup goroutine using the package logger
	go cleanupReassemblyBuffers(ensureLogger())
}

// GetUDPReadBufferPool returns the package's UDP read buffer pool.
func GetUDPReadBufferPool() *sync.Pool {
	return &udpReadBufferPool
}

// generateMessageID generates a unique message ID randomly.
func generateMessageID() uint32 {
	return rand.Uint32()
}

// FragmentData divides the data into UDP fragments based on the configured MaxFragmentSize.
// (No changes needed in FragmentData for sharding)
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

	numFragments := (len(data) + maxFragmentPayloadSize - 1) / maxFragmentPayloadSize
	if numFragments == 0 && len(data) > 0 {
		numFragments = 1
	}

	if numFragments > 65535 {
		return nil, fmt.Errorf("message too large (%d bytes) to be fragmented into <= 65535 fragments with fragment size %d", len(data), maxFragmentPayloadSize)
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
			Flags:          0,
		}
		if i == numFragments-1 {
			header.Flags |= FlagLastFragment
		}

		headerBuf := fragmentHeaderBufferPool.Get().(*bytes.Buffer)
		headerBuf.Reset()

		err := binary.Write(headerBuf, binary.BigEndian, header)
		if err != nil {
			fragmentHeaderBufferPool.Put(headerBuf)
			return nil, fmt.Errorf("failed to write fragment header %d for message %d: %w", i, messageID, err)
		}

		packet := make([]byte, FragmentHeaderSize+len(fragmentData))
		copy(packet[:FragmentHeaderSize], headerBuf.Bytes())
		copy(packet[FragmentHeaderSize:], fragmentData)

		fragments = append(fragments, packet)
		fragmentHeaderBufferPool.Put(headerBuf)
	}

	return fragments, nil
}

// ReassembleData attempts to reassemble UDP fragments into the original data.
// Uses sharding to reduce mutex contention.
func ReassembleData(packet []byte) ([]byte, error) {
	logger := ensureLogger()
	if len(packet) < FragmentHeaderSize {
		return nil, nil // Not our fragment
	}

	headerBuf := bytes.NewReader(packet[:FragmentHeaderSize])
	var header FragmentHeader
	if err := binary.Read(headerBuf, binary.BigEndian, &header); err != nil {
		return nil, fmt.Errorf("failed to read fragment header: %w", err)
	}
	fragmentPayload := packet[FragmentHeaderSize:]

	// Basic header validation
	if header.TotalFragments == 0 {
		return nil, fmt.Errorf("invalid fragment header: TotalFragments is zero (ID %d)", header.MessageID)
	}
	if header.FragmentNumber >= header.TotalFragments {
		return nil, fmt.Errorf("invalid fragment number %d >= TotalFragments %d (ID %d)", header.FragmentNumber, header.TotalFragments, header.MessageID)
	}

	// Determine which shard to use
	shardIndex := header.MessageID % numShards
	shard := &shards[shardIndex] // Get pointer to the shard

	shard.mutex.Lock() // Lock the specific shard

	msg, ok := shard.buffers[header.MessageID]
	if !ok {
		// *** START: Global Limit Check for New Messages ***
		currentCount := totalReassemblyCount.Load()
		if currentCount >= globalConfig.MaxConcurrentReassemblies {
			shard.mutex.Unlock() // Unlock before logging/returning
			logger.Printf("Warning: Global max concurrent reassemblies limit (%d) reached. Dropping fragment for new message ID %d.", globalConfig.MaxConcurrentReassemblies, header.MessageID)
			return nil, ErrReassemblyLimitExceeded // Return specific error
		}
		// *** END: Global Limit Check ***

		// Limit not reached, proceed to create the buffer for the new message ID
		if header.TotalFragments > 65535 {
			shard.mutex.Unlock()
			return nil, fmt.Errorf("invalid TotalFragments %d (ID %d)", header.TotalFragments, header.MessageID)
		}
		// Log current total count when adding a new buffer
		logger.Printf("First fragment received for message %d (expecting %d total, current global buffers: %d)", header.MessageID, header.TotalFragments, currentCount)
		msg = &fragmentedMessage{
			fragmentsReceived: make([][]byte, header.TotalFragments),
			lastFragmentTime:  time.Now(),
			totalFragments:    header.TotalFragments,
		}
		shard.buffers[header.MessageID] = msg
		totalReassemblyCount.Add(1) // Increment global count *after* adding to map
	} else {
		// Existing message buffer
		if header.TotalFragments != msg.totalFragments {
			// Mismatch in total fragments count within the same message ID - corruption likely
			logger.Printf("Error: TotalFragments mismatch for message %d (received %d, expected %d). Discarding buffer.", header.MessageID, header.TotalFragments, msg.totalFragments)
			delete(shard.buffers, header.MessageID) // Discard inconsistent buffer
			totalReassemblyCount.Add(-1)            // Decrement count as we removed it
			shard.mutex.Unlock()                    // Unlock before returning
			return nil, fmt.Errorf("total fragment count mismatch for message ID %d", header.MessageID)
		}
	}

	// Store the fragment payload if we haven't received this one yet
	if msg.fragmentsReceived[header.FragmentNumber] == nil {
		payloadCopy := make([]byte, len(fragmentPayload))
		copy(payloadCopy, fragmentPayload)
		msg.fragmentsReceived[header.FragmentNumber] = payloadCopy
	} else {
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

	if receivedCount == int(msg.totalFragments) {
		// All fragments received! Attempt assembly.
		logger.Printf("All fragments received for message %d. Assembling...", header.MessageID)
		fragmentsToAssemble := msg.fragmentsReceived
		delete(shard.buffers, header.MessageID) // Remove from shard map
		totalReassemblyCount.Add(-1)            // Decrement global count
		shard.mutex.Unlock()                    // *** Unlock shard BEFORE assembly ***

		// --- Assembly logic (outside lock) ---
		totalSize := 0
		for _, frag := range fragmentsToAssemble {
			if frag != nil {
				totalSize += len(frag)
			}
		}

		if totalSize > MaxUDPSize*int(header.TotalFragments) {
			return nil, fmt.Errorf("reassembled size %d exceeds theoretical maximum for message %d", totalSize, header.MessageID)
		}

		reassembledData := make([]byte, totalSize)
		offset := 0
		for i := 0; i < int(header.TotalFragments); i++ {
			frag := fragmentsToAssemble[i]
			if frag == nil {
				return nil, fmt.Errorf("internal error: missing fragment %d during final assembly of message %d", i, header.MessageID)
			}
			copy(reassembledData[offset:], frag)
			offset += len(frag)
			fragmentsToAssemble[i] = nil // Allow GC
		}

		logger.Printf("Message %d assembled successfully (%d bytes).", header.MessageID, totalSize)
		return reassembledData, nil
		// --- End Assembly logic ---
	}

	// Reassembly incomplete, wait for more fragments
	shard.mutex.Unlock() // Unlock shard if assembly wasn't complete
	return nil, nil
}

// cleanupReassemblyBuffers iterates through all shards to clean up timed-out buffers.
func cleanupReassemblyBuffers(logger *CondLogger) {
	interval := globalConfig.CleanupInterval
	if interval <= 0 {
		logger.Printf("Warning: CleanupInterval is zero or negative (%s), using default %s", interval, DefaultCleanupInterval)
		interval = DefaultCleanupInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Printf("Cleanup goroutine started with interval %s for %d shards", interval, numShards)

	for range ticker.C {
		now := time.Now()
		timeout := globalConfig.ReassemblyTimeout
		if timeout <= 0 {
			logger.Printf("Warning: ReassemblyTimeout is zero or negative (%s), using default %s for cleanup check", timeout, DefaultReassemblyTimeout)
			timeout = DefaultReassemblyTimeout
		}

		totalRemoved := 0
		// Iterate through each shard
		for i := range shards {
			shard := &shards[i]
			shardRemovedCount := 0

			shard.mutex.Lock() // Lock the current shard
			for msgID, msg := range shard.buffers {
				if now.Sub(msg.lastFragmentTime) > timeout {
					// Log removal details
					receivedCount := 0
					for _, frag := range msg.fragmentsReceived {
						if frag != nil {
							receivedCount++
						}
					}
					// Log before deleting, include shard index for debugging
					logger.Printf("Cleanup[Shard %d]: Removing timed-out message buffer for ID %d (%d/%d fragments received, last activity: %s)",
						i, msgID, receivedCount, msg.totalFragments, msg.lastFragmentTime.Format(time.RFC3339))

					delete(shard.buffers, msgID)
					totalReassemblyCount.Add(-1) // Decrement global count
					shardRemovedCount++
				}
			}
			shard.mutex.Unlock() // Unlock the current shard

			totalRemoved += shardRemovedCount
		} // End loop through shards

		if totalRemoved > 0 {
			currentTotal := totalReassemblyCount.Load() // Get current total after cleanup
			logger.Printf("Cleanup: Removed %d total timed-out message buffers across all shards. Current global buffers: %d", totalRemoved, currentTotal)
		}
	}
	logger.Printf("Cleanup goroutine finished.") // Should ideally not happen unless program exits
}

// ExampleMessageHandler (No changes needed)
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
