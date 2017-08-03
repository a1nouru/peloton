package task

import (
	"sync"

	log "github.com/sirupsen/logrus"

	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"

	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"
	"github.com/pkg/errors"
	"github.com/uber-go/tally"
)

// Tracker is the interface for resource manager to
// track all the tasks in rm
type Tracker interface {

	// AddTask adds the task to state machine
	AddTask(
		t *resmgr.Task,
		handler *eventstream.Handler,
		respool respool.ResPool,
		config *Config) error

	// GetTask gets the RM task for taskID
	GetTask(t *peloton.TaskID) *RMTask

	// Sets the hostname where the task is placed.
	SetPlacement(t *peloton.TaskID, hostname string)

	// SetPlacementHost Sets the hostname for the placement
	SetPlacementHost(placement *resmgr.Placement, hostname string)

	// DeleteTask deletes the task from the map
	DeleteTask(t *peloton.TaskID)

	// MarkItDone marks the task done and add back those
	// resources to respool
	MarkItDone(taskID *peloton.TaskID) error

	// TasksByHosts returns all tasks of the given type running on the given hosts.
	TasksByHosts(hosts []string, taskType resmgr.TaskType) map[string][]*RMTask

	// AddResources adds the task resources to respool
	AddResources(taskID *peloton.TaskID) error

	// GetSize returns the number of the tasks in tracker
	GetSize() int64

	// Clear cleans the tracker with all the tasks
	Clear()

	// GetActiveTasks returns task states map
	GetActiveTasks(jobID string, respoolID string) map[string]string
}

// tracker is the rmtask tracker
// map[taskid]*rmtask
type tracker struct {
	sync.Mutex

	// Maps task id -> rm task
	tasks map[string]*RMTask

	// Maps hostname -> task type -> task id -> rm task
	placements map[string]map[resmgr.TaskType]map[string]*RMTask

	metrics *Metrics
}

// singleton object
var rmtracker *tracker

// InitTaskTracker initialize the task tracker
func InitTaskTracker(parent tally.Scope) {
	if rmtracker != nil {
		log.Info("Resource Manager Tracker is already initialized")
		return
	}
	rmtracker = &tracker{
		tasks:      make(map[string]*RMTask),
		placements: map[string]map[resmgr.TaskType]map[string]*RMTask{},
		metrics:    NewMetrics(parent.SubScope("tracker")),
	}
	log.Info("Resource Manager Tracker is initialized")
}

// GetTracker gets the singelton object of the tracker
func GetTracker() Tracker {
	if rmtracker == nil {
		log.Fatal("Tracker is not initialized")
	}
	return rmtracker
}

// AddTask adds task to resmgr task tracker
func (tr *tracker) AddTask(
	t *resmgr.Task,
	handler *eventstream.Handler,
	respool respool.ResPool,
	config *Config) error {
	tr.Lock()
	defer tr.Unlock()
	rmTask, err := CreateRMTask(t, handler, respool, config)
	if err != nil {
		return err
	}
	tr.tasks[rmTask.task.Id.Value] = rmTask
	if rmTask.task.Hostname != "" {
		tr.setPlacement(rmTask.task.Id, rmTask.task.Hostname)
	}
	tr.metrics.TaskLeninTracker.Update(float64(tr.GetSize()))
	return nil
}

// GetTask gets the RM task for taskID
func (tr *tracker) GetTask(t *peloton.TaskID) *RMTask {
	tr.Lock()
	defer tr.Unlock()
	if rmTask, ok := tr.tasks[t.Value]; ok {
		return rmTask
	}
	return nil
}

func (tr *tracker) setPlacement(t *peloton.TaskID, hostname string) {
	rmTask, ok := tr.tasks[t.Value]
	if !ok {
		return
	}
	tr.clearPlacement(rmTask)
	rmTask.task.Hostname = hostname
	if _, exists := tr.placements[hostname]; !exists {
		tr.placements[hostname] = map[resmgr.TaskType]map[string]*RMTask{}
	}
	if _, exists := tr.placements[hostname][rmTask.task.Type]; !exists {
		tr.placements[hostname][rmTask.task.Type] = map[string]*RMTask{}
	}
	if _, exists := tr.placements[hostname][rmTask.task.Type][t.Value]; !exists {
		tr.placements[hostname][rmTask.task.Type][t.Value] = rmTask
	}
}

// clearPlacement will remove the task from the placements map.
func (tr *tracker) clearPlacement(rmTask *RMTask) {
	if rmTask.task.Hostname == "" {
		return
	}
	delete(tr.placements[rmTask.task.Hostname][rmTask.task.Type], rmTask.task.Id.Value)
	if len(tr.placements[rmTask.task.Hostname][rmTask.task.Type]) == 0 {
		delete(tr.placements[rmTask.task.Hostname], rmTask.task.Type)
	}
	if len(tr.placements[rmTask.task.Hostname]) == 0 {
		delete(tr.placements, rmTask.task.Hostname)
	}
}

// SetPlacement will set the hostname that the task is currently placed on.
func (tr *tracker) SetPlacement(t *peloton.TaskID, hostname string) {
	tr.Lock()
	defer tr.Unlock()
	tr.setPlacement(t, hostname)
}

// SetPlacementHost will set the hostname that the task is currently placed on.
func (tr *tracker) SetPlacementHost(placement *resmgr.Placement, hostname string) {
	tr.Lock()
	defer tr.Unlock()
	for _, t := range placement.GetTasks() {
		tr.setPlacement(t, hostname)
	}
}

// DeleteTask deletes the task from the map
func (tr *tracker) DeleteTask(t *peloton.TaskID) {
	tr.Lock()
	defer tr.Unlock()
	if rmTask, exists := tr.tasks[t.Value]; exists {
		tr.clearPlacement(rmTask)
	}
	delete(tr.tasks, t.Value)
	tr.metrics.TaskLeninTracker.Update(float64(tr.GetSize()))
}

// MarkItDone updates the resources in resmgr
func (tr *tracker) MarkItDone(
	tID *peloton.TaskID) error {
	task := tr.GetTask(tID)
	if task == nil {
		return errors.Errorf("task %s is not in tracker", tID)
	}
	err := task.respool.SubtractFromAllocation(
		scalar.ConvertToResmgrResource(
			task.task.GetResource()))
	if err != nil {
		return errors.Errorf("Not able to update task %s ", tID)
	}
	log.WithField("Task", tID.Value).Info("Deleting the task from Tracker")
	tr.DeleteTask(tID)
	return nil
}

// TasksByHosts returns all tasks of the given type running on the given hosts.
func (tr *tracker) TasksByHosts(hosts []string, taskType resmgr.TaskType) map[string][]*RMTask {
	result := map[string][]*RMTask{}
	types := []resmgr.TaskType{}
	if taskType == resmgr.TaskType_UNKNOWN {
		for t := range resmgr.TaskType_name {
			types = append(types, resmgr.TaskType(t))
		}
	} else {
		types = append(types, taskType)
	}
	for _, hostname := range hosts {
		for _, tType := range types {
			for _, rmTask := range tr.placements[hostname][tType] {
				result[hostname] = append(result[hostname], rmTask)
			}
		}
	}
	return result
}

// AddResources adds the task resources to respool
func (tr *tracker) AddResources(
	tID *peloton.TaskID) error {
	task := tr.GetTask(tID)
	if task == nil {
		return errors.Errorf("task %s is not in tracker", tID)
	}
	res := scalar.ConvertToResmgrResource(task.task.GetResource())
	err := task.respool.AddToAllocation(res)
	if err != nil {
		return errors.Errorf("Not able to add resources for "+
			"task %s for respool %s ", tID, task.respool.Name())
	}
	log.WithFields(log.Fields{
		"Respool":   task.respool.Name(),
		"Resources": res,
	}).Debug("Added resources to Respool")
	return nil
}

// GetSize gets the number of tasks in tracker
func (tr *tracker) GetSize() int64 {
	return int64(len(tr.tasks))
}

// Clear cleans the tracker with all the existing tasks
func (tr *tracker) Clear() {
	tr.Lock()
	defer tr.Unlock()
	// Cleaning the tasks
	for k := range tr.tasks {
		delete(tr.tasks, k)
	}
	// Cleaning the placements
	for k := range tr.placements {
		delete(tr.placements, k)
	}
}

// GetActiveTasks returns task to states map, if jobID or respoolID is provided,
// only tasks for that job or respool will be returned
func (tr *tracker) GetActiveTasks(jobID string, respoolID string) map[string]string {
	var taskStates = map[string]string{}
	for id, task := range tr.tasks {
		if jobID == "" && respoolID == "" {
			taskStates[id] = task.GetCurrentState().String()
		} else {
			// TODO: make it handle jobID 'AND' respoolID if there is a usecase
			if task.Task().GetJobId().GetValue() == jobID {
				taskStates[id] = task.GetCurrentState().String()
			}

			if task.Respool().ID() == respoolID {
				taskStates[id] = task.GetCurrentState().String()
			}
		}
	}
	return taskStates
}
