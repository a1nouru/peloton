package queue

import (
	"container/list"
	"errors"

	"code.uber.internal/infra/peloton/.gen/peloton/api/respool"
)

// Queue is the interface implemented by all the the queues
type Queue interface {
	// Enqueue queues a gang (task list gang) based on its priority into FIFO queue
	Enqueue(tlist *list.List) error
	// Dequeue dequeues the gang (task list gang) based on the priority and order
	// they came into the queue
	Dequeue() (*list.List, error)
	// Peek peeks the gang(list) based on the priority and order
	// they came into the queue
	// It will return an error if there is no gang in the queue
	Peek() (*list.List, error)
	// Remove removes the item from the queue
	Remove(item *list.List) error
}

// CreateQueue is factory method to create the specified queue
func CreateQueue(policy respool.SchedulingPolicy, limit int64) (Queue, error) {
	// Factory method to create specific queue object based on policy
	switch policy {
	case respool.SchedulingPolicy_PriorityFIFO:
		return NewPriorityQueue(limit), nil
	default:
		//if type is invalid, return an error
		return nil, errors.New("Invalid queue Type")
	}
}
