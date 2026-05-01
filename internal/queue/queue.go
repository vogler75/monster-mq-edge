package queue

import "monstermq.io/edge/internal/stores"

// MessageQueue buffers broker messages before forwarding them to an external
// target. PollBlock keeps the returned block pending until PollCommit is called.
type MessageQueue interface {
	IsQueueFull() bool
	Size() int
	Capacity() int
	Add(message stores.BrokerMessage)
	PollBlock(handler func(stores.BrokerMessage)) int
	PollCommit()
	Close() error
}
