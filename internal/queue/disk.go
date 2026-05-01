package queue

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

const (
	diskHeaderSize      int64 = 8
	diskBytesPerMessage       = 2048
)

// DiskQueue mirrors the JVM MessageQueueDisk design: a fixed-size circular
// file with persisted read/write positions and explicit block commits.
type DiskQueue struct {
	logger      *slog.Logger
	file        *os.File
	fileSize    int64
	capacity    int
	blockSize   int
	pollTimeout time.Duration

	mu          sync.Mutex
	wake        chan struct{}
	readPos     int64
	writePos    int64
	queueFull   bool
	outputBlock []stores.BrokerMessage
}

func NewDiskQueue(queueName, deviceName string, logger *slog.Logger, queueSize, blockSize int, pollTimeout time.Duration, diskPath string) (*DiskQueue, error) {
	if queueSize <= 0 {
		queueSize = 5000
	}
	if blockSize <= 0 || blockSize > queueSize {
		blockSize = min(100, queueSize)
	}
	if pollTimeout <= 0 {
		pollTimeout = 100 * time.Millisecond
	}
	if diskPath == "" {
		diskPath = "./buffer/mqttbridge"
	}
	if err := os.MkdirAll(diskPath, 0o755); err != nil {
		return nil, err
	}

	fileName := filepath.Join(diskPath, fmt.Sprintf("%s-%s.buf", queueName, deviceName))
	fileSize := int64(queueSize * diskBytesPerMessage)
	file, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	q := &DiskQueue{
		logger:      logger,
		file:        file,
		fileSize:    fileSize,
		capacity:    queueSize,
		blockSize:   blockSize,
		pollTimeout: pollTimeout,
		wake:        make(chan struct{}, 1),
		readPos:     diskHeaderSize,
		writePos:    diskHeaderSize,
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Size() != fileSize {
		if err := file.Truncate(fileSize); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := q.writeHeaderLocked(); err != nil {
			_ = file.Close()
			return nil, err
		}
	} else if err := q.readHeaderLocked(); err != nil {
		_ = file.Close()
		return nil, err
	}

	if logger != nil {
		logger.Info("disk queue opened", "file", fileName, "sizeBytes", fileSize, "readPosition", q.readPos, "writePosition", q.writePos)
	}
	return q, nil
}

func (q *DiskQueue) IsQueueFull() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queueFull
}

func (q *DiskQueue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	size := q.writePos - q.readPos
	if size >= 0 {
		return int(size / diskBytesPerMessage)
	}
	return int((q.fileSize - diskHeaderSize + size) / diskBytesPerMessage)
}

func (q *DiskQueue) Capacity() int { return q.capacity }

func (q *DiskQueue) Add(message stores.BrokerMessage) {
	q.mu.Lock()
	ok := q.enqueueLocked(message)
	if ok && q.queueFull {
		q.queueFull = false
		if q.logger != nil {
			q.logger.Warn("disk queue not full anymore", "size", q.sizeLocked(), "capacity", q.Capacity())
		}
	}
	if !ok && !q.queueFull {
		q.queueFull = true
		if q.logger != nil {
			q.logger.Warn("disk queue is full; dropping message", "size", q.sizeLocked(), "capacity", q.Capacity(), "topic", message.TopicName)
		}
	}
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *DiskQueue) PollBlock(handler func(stores.BrokerMessage)) int {
	q.mu.Lock()
	if len(q.outputBlock) > 0 {
		block := append([]stores.BrokerMessage(nil), q.outputBlock...)
		q.mu.Unlock()
		time.Sleep(time.Second)
		for _, message := range block {
			handler(message)
		}
		return len(block)
	}

	message, err := q.dequeueLocked()
	q.mu.Unlock()
	if errors.Is(err, errQueueEmpty) {
		timer := time.NewTimer(q.pollTimeout)
		select {
		case <-q.wake:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		q.mu.Lock()
		message, err = q.dequeueLocked()
		q.mu.Unlock()
	}
	if err != nil {
		return 0
	}

	q.mu.Lock()
	q.outputBlock = append(q.outputBlock, message)
	q.mu.Unlock()
	handler(message)

	for i := 1; i < q.blockSize; i++ {
		q.mu.Lock()
		next, err := q.dequeueLocked()
		if err == nil {
			q.outputBlock = append(q.outputBlock, next)
		}
		q.mu.Unlock()
		if err != nil {
			break
		}
		handler(next)
	}

	q.mu.Lock()
	size := len(q.outputBlock)
	q.mu.Unlock()
	return size
}

func (q *DiskQueue) PollCommit() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.outputBlock = nil
	if err := q.writeReadPositionLocked(); err != nil && q.logger != nil {
		q.logger.Warn("disk queue commit failed", "err", err)
	}
	if err := q.file.Sync(); err != nil && q.logger != nil {
		q.logger.Warn("disk queue sync failed", "err", err)
	}
}

func (q *DiskQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.file == nil {
		return nil
	}
	err := q.file.Sync()
	if closeErr := q.file.Close(); err == nil {
		err = closeErr
	}
	q.file = nil
	return err
}

var errQueueEmpty = errors.New("queue empty")

func (q *DiskQueue) enqueueLocked(message stores.BrokerMessage) bool {
	data, err := json.Marshal(message)
	if err != nil {
		if q.logger != nil {
			q.logger.Warn("disk queue message serialization failed", "topic", message.TopicName, "err", err)
		}
		return false
	}
	dataSize := int64(len(data) + 4)
	if dataSize > q.fileSize-diskHeaderSize {
		if q.logger != nil {
			q.logger.Warn("disk queue message too large", "topic", message.TopicName, "bytes", len(data))
		}
		return false
	}

	if q.writePos+dataSize > q.fileSize {
		if err := q.writeUint32AtLocked(q.writePos, 0); err != nil {
			return false
		}
		q.writePos = diskHeaderSize
	}
	if q.writePos < q.readPos && q.writePos+dataSize >= q.readPos {
		return false
	}
	if q.writePos == q.readPos && q.sizeLocked() > 0 {
		return false
	}

	if err := q.writeUint32AtLocked(q.writePos, uint32(len(data))); err != nil {
		return false
	}
	if _, err := q.file.WriteAt(data, q.writePos+4); err != nil {
		return false
	}
	q.writePos += dataSize
	return q.writeWritePositionLocked() == nil
}

func (q *DiskQueue) dequeueLocked() (stores.BrokerMessage, error) {
	var zero stores.BrokerMessage
	if q.readPos == q.writePos {
		return zero, errQueueEmpty
	}

	dataSize, err := q.readUint32AtLocked(q.readPos)
	if err != nil {
		return zero, err
	}
	if dataSize == 0 {
		q.readPos = diskHeaderSize
		if q.writePos == diskHeaderSize {
			return zero, errQueueEmpty
		}
		dataSize, err = q.readUint32AtLocked(q.readPos)
		if err != nil {
			return zero, err
		}
	}

	data := make([]byte, dataSize)
	if _, err := q.file.ReadAt(data, q.readPos+4); err != nil && err != io.EOF {
		return zero, err
	}
	var message stores.BrokerMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return zero, err
	}
	q.readPos += int64(dataSize) + 4
	return message, nil
}

func (q *DiskQueue) readHeaderLocked() error {
	readPos, err := q.readUint32AtLocked(0)
	if err != nil {
		return err
	}
	writePos, err := q.readUint32AtLocked(4)
	if err != nil {
		return err
	}
	if readPos < uint32(diskHeaderSize) || int64(readPos) >= q.fileSize || writePos < uint32(diskHeaderSize) || int64(writePos) >= q.fileSize {
		q.readPos = diskHeaderSize
		q.writePos = diskHeaderSize
		return q.writeHeaderLocked()
	}
	q.readPos = int64(readPos)
	q.writePos = int64(writePos)
	return nil
}

func (q *DiskQueue) writeHeaderLocked() error {
	if err := q.writeReadPositionLocked(); err != nil {
		return err
	}
	return q.writeWritePositionLocked()
}

func (q *DiskQueue) writeReadPositionLocked() error {
	return q.writeUint32AtLocked(0, uint32(q.readPos))
}

func (q *DiskQueue) writeWritePositionLocked() error {
	return q.writeUint32AtLocked(4, uint32(q.writePos))
}

func (q *DiskQueue) readUint32AtLocked(offset int64) (uint32, error) {
	var buf [4]byte
	if _, err := q.file.ReadAt(buf[:], offset); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

func (q *DiskQueue) writeUint32AtLocked(offset int64, value uint32) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	_, err := q.file.WriteAt(buf[:], offset)
	return err
}

func (q *DiskQueue) sizeLocked() int {
	size := q.writePos - q.readPos
	if size >= 0 {
		return int(size / diskBytesPerMessage)
	}
	return int((q.fileSize - diskHeaderSize + size) / diskBytesPerMessage)
}
