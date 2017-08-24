package resmgr

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"

	"go.uber.org/yarpc"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/common/queue"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"
	rmtask "code.uber.internal/infra/peloton/resmgr/task"
	"code.uber.internal/infra/peloton/util"

	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	t "code.uber.internal/infra/peloton/.gen/peloton/api/task"
	pb_eventstream "code.uber.internal/infra/peloton/.gen/peloton/private/eventstream"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
)

var (
	errFailingGangMemberTask = errors.New("task fail because other gang member failed")
	errSameTaskPresent       = errors.New("same task present in tracker, Ignoring new task")
	errGangNotEnqueued       = errors.New("Could not enqueue gang to ready after retry")
	errEnqueuedAgain         = errors.New("enqueued again after retry")
)

// ServiceHandler implements peloton.private.resmgr.ResourceManagerService
// TODO: add placing and placed task queues
type ServiceHandler struct {
	metrics            *Metrics
	resPoolTree        respool.Tree
	placements         queue.Queue
	eventStreamHandler *eventstream.Handler
	rmTracker          rmtask.Tracker
	maxOffset          *uint64
	config             Config
}

// InitServiceHandler initializes the handler for ResourceManagerService
func InitServiceHandler(
	d *yarpc.Dispatcher,
	parent tally.Scope,
	rmTracker rmtask.Tracker,
	conf Config) *ServiceHandler {

	var maxOffset uint64
	handler := &ServiceHandler{
		metrics:     NewMetrics(parent.SubScope("resmgr")),
		resPoolTree: respool.GetTree(),
		placements: queue.NewQueue(
			"placement-queue",
			reflect.TypeOf(resmgr.Placement{}),
			maxPlacementQueueSize,
		),
		rmTracker: rmTracker,
		maxOffset: &maxOffset,
		config:    conf,
	}
	// TODO: move eventStreamHandler buffer size into config
	handler.eventStreamHandler = initEventStreamHandler(d, 1000, parent.SubScope("resmgr"))

	d.Register(resmgrsvc.BuildResourceManagerServiceYARPCProcedures(handler))
	return handler
}

func initEventStreamHandler(d *yarpc.Dispatcher, bufferSize int, parentScope tally.Scope) *eventstream.Handler {
	eventStreamHandler := eventstream.NewEventStreamHandler(
		bufferSize,
		[]string{
			common.PelotonJobManager,
			common.PelotonResourceManager,
		},
		nil,
		parentScope)

	d.Register(pb_eventstream.BuildEventStreamServiceYARPCProcedures(eventStreamHandler))

	return eventStreamHandler
}

// GetStreamHandler returns the stream handler
func (h *ServiceHandler) GetStreamHandler() *eventstream.Handler {
	return h.eventStreamHandler
}

// EnqueueGangs implements ResourceManagerService.EnqueueGangs
func (h *ServiceHandler) EnqueueGangs(
	ctx context.Context,
	req *resmgrsvc.EnqueueGangsRequest,
) (*resmgrsvc.EnqueueGangsResponse, error) {

	log.WithField("request", req).Info("EnqueueGangs called.")
	h.metrics.APIEnqueueGangs.Inc(1)

	// Lookup respool from the resource pool tree
	respoolID := req.GetResPool()
	respool, err := respool.GetTree().Get(respoolID)
	if err != nil {
		h.metrics.EnqueueGangFail.Inc(1)
		return &resmgrsvc.EnqueueGangsResponse{
			Error: &resmgrsvc.EnqueueGangsResponse_Error{
				NotFound: &resmgrsvc.ResourcePoolNotFound{
					Id:      respoolID,
					Message: err.Error(),
				},
			},
		}, nil
	}
	// TODO: check if the user has permission to run tasks in the
	// respool

	// Enqueue the gangs sent in an API call to the pending queue of the respool.
	// For each gang, add its tasks to the state machine, enqueue the gang, and
	// return per-task success/failure.
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	for _, gang := range req.GetGangs() {
		failed, err = h.enqueueGang(gang, respool)
		// Report per-task success/failure for all tasks in gang
		for _, task := range gang.GetTasks() {
			if err != nil {
				failed = append(
					failed,
					&resmgrsvc.EnqueueGangsFailure_FailedTask{
						Task:    task,
						Message: err.Error(),
					},
				)
				h.metrics.EnqueueGangFail.Inc(1)
			} else {
				h.metrics.EnqueueGangSuccess.Inc(1)
			}
		}
	}

	if len(failed) > 0 {
		return &resmgrsvc.EnqueueGangsResponse{
			Error: &resmgrsvc.EnqueueGangsResponse_Error{
				Failure: &resmgrsvc.EnqueueGangsFailure{
					Failed: failed,
				},
			},
		}, nil
	}

	response := resmgrsvc.EnqueueGangsResponse{}
	log.Debug("Enqueue Returned")
	return &response, nil
}

func (h *ServiceHandler) enqueueGang(
	gang *resmgrsvc.Gang,
	respool respool.ResPool) (
	[]*resmgrsvc.EnqueueGangsFailure_FailedTask,
	error) {
	totalGangResources := &scalar.Resources{}
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	var err error
	for _, task := range gang.GetTasks() {
		err := h.requeueTask(task)
		if err != nil {
			failed = append(
				failed,
				&resmgrsvc.EnqueueGangsFailure_FailedTask{
					Task:    task,
					Message: err.Error(),
				},
			)
			continue
		}

		// Adding task to state machine
		err = h.rmTracker.AddTask(
			task,
			h.eventStreamHandler,
			respool,
			h.config.RmTaskConfig,
		)
		if err != nil {
			failed = append(
				failed,
				&resmgrsvc.EnqueueGangsFailure_FailedTask{
					Task:    task,
					Message: err.Error(),
				},
			)
			continue
		}
		totalGangResources = totalGangResources.Add(
			scalar.ConvertToResmgrResource(
				task.GetResource()))

		if h.rmTracker.GetTask(task.Id) != nil {
			err = h.rmTracker.GetTask(task.Id).TransitTo(
				t.TaskState_PENDING.String())
			if err != nil {
				log.Error(err)
			}
		}
	}
	if len(failed) == 0 {
		err = respool.EnqueueGang(gang)
		if err == nil {
			err = respool.AddToDemand(totalGangResources)
			log.WithFields(log.Fields{
				"TotalResourcesAdded": totalGangResources,
				"Respool":             respool.Name(),
			}).Debug("Resources added for Gang")
			if err != nil {
				log.Error(err)
			}
		} else {
			// We need to remove gang tasks from tracker
			h.removeTasksFromTracker(gang)
		}
	} else {
		err = errFailingGangMemberTask
	}
	return failed, err
}

// removeTasksFromTracker removes the  task from the tracker
func (h *ServiceHandler) removeTasksFromTracker(gang *resmgrsvc.Gang) {
	for _, task := range gang.Tasks {
		h.rmTracker.DeleteTask(task.Id)
	}
}

// requeueTask validates the enqueued task has the same mesos task id or not
// If task has same mesos task id => return error
// If task has different mesos task id then check state and based on the state
// act accordingly
func (h *ServiceHandler) requeueTask(requeuedTask *resmgr.Task) error {
	rmTask := h.rmTracker.GetTask(requeuedTask.Id)
	if rmTask == nil {
		return nil
	}

	if *requeuedTask.TaskId.Value == *rmTask.Task().TaskId.Value {
		return errSameTaskPresent
	}

	currentTaskState := rmTask.GetCurrentState()

	// If state is Launching or Running then only
	// put task to ready queue with update of
	// mesos task id otherwise ignore
	if currentTaskState == t.TaskState_LAUNCHING ||
		currentTaskState == t.TaskState_RUNNING {
		// Updating the New Mesos Task ID
		rmTask.Task().TaskId = requeuedTask.TaskId
		// Transitioning back to Ready State
		rmTask.TransitTo(t.TaskState_READY.String())
		// Adding to ready Queue
		var tasks []*resmgr.Task
		gang := &resmgrsvc.Gang{
			Tasks: append(tasks, rmTask.Task()),
		}
		err := rmtask.GetScheduler().EnqueueGang(gang)
		if err != nil {
			log.WithField("Gang", gang).Error(errGangNotEnqueued.Error())
			return err
		}
		log.WithField("Gang", gang).Debug(errEnqueuedAgain.Error())
		return errEnqueuedAgain
	}
	return errSameTaskPresent
}

// DequeueGangs implements ResourceManagerService.DequeueGangs
func (h *ServiceHandler) DequeueGangs(
	ctx context.Context,
	req *resmgrsvc.DequeueGangsRequest,
) (*resmgrsvc.DequeueGangsResponse, error) {

	h.metrics.APIDequeueGangs.Inc(1)

	limit := req.GetLimit()
	timeout := time.Duration(req.GetTimeout())
	sched := rmtask.GetScheduler()

	var gangs []*resmgrsvc.Gang
	for i := uint32(0); i < limit; i++ {
		gang, err := sched.DequeueGang(timeout*time.Millisecond, req.Type)
		if err != nil {
			log.Debug("Timeout to dequeue gang from ready queue")
			h.metrics.DequeueGangTimeout.Inc(1)
			break
		}
		tasksToRemove := make(map[string]*resmgr.Task)
		for _, task := range gang.GetTasks() {
			h.metrics.DequeueGangSuccess.Inc(1)

			// Moving task to Placing state
			if h.rmTracker.GetTask(task.Id) != nil {
				err = h.rmTracker.GetTask(task.Id).TransitTo(
					t.TaskState_PLACING.String())
				if err != nil {
					log.WithError(err).WithField(
						"taskID", task.Id.Value).
						Error("Failed to transit state " +
							"for task")
				}
			} else {
				tasksToRemove[task.Id.Value] = task
			}
		}
		gang = h.removeFromGang(gang, tasksToRemove)
		gangs = append(gangs, gang)
	}
	// TODO: handle the dequeue errors better
	response := resmgrsvc.DequeueGangsResponse{Gangs: gangs}
	log.WithField("response", response).Debug("DequeueGangs succeeded")
	return &response, nil
}

func (h *ServiceHandler) removeFromGang(
	gang *resmgrsvc.Gang,
	tasksToRemove map[string]*resmgr.Task) *resmgrsvc.Gang {
	if len(tasksToRemove) == 0 {
		return gang
	}
	var newTasks []*resmgr.Task
	for _, gt := range gang.GetTasks() {
		if _, ok := tasksToRemove[gt.Id.Value]; !ok {
			newTasks = append(newTasks, gt)
		}
	}
	gang.Tasks = newTasks
	return gang
}

// SetPlacements implements ResourceManagerService.SetPlacements
func (h *ServiceHandler) SetPlacements(
	ctx context.Context,
	req *resmgrsvc.SetPlacementsRequest,
) (*resmgrsvc.SetPlacementsResponse, error) {

	log.WithField("request", req).Debug("SetPlacements called.")
	h.metrics.APISetPlacements.Inc(1)

	var failed []*resmgrsvc.SetPlacementsFailure_FailedPlacement
	var err error
	for _, placement := range req.GetPlacements() {
		newplacement := h.transitTasksInPlacement(placement,
			t.TaskState_PLACING,
			t.TaskState_PLACED)
		h.rmTracker.SetPlacementHost(newplacement, newplacement.Hostname)
		err = h.placements.Enqueue(newplacement)
		if err != nil {
			log.WithField("placement", newplacement).
				WithError(err).Error("Failed to enqueue placement")
			failed = append(
				failed,
				&resmgrsvc.SetPlacementsFailure_FailedPlacement{
					Placement: newplacement,
					Message:   err.Error(),
				},
			)
			h.metrics.SetPlacementFail.Inc(1)
		} else {
			h.metrics.SetPlacementSuccess.Inc(1)
		}
	}

	if len(failed) > 0 {
		return &resmgrsvc.SetPlacementsResponse{
			Error: &resmgrsvc.SetPlacementsResponse_Error{
				Failure: &resmgrsvc.SetPlacementsFailure{
					Failed: failed,
				},
			},
		}, nil
	}
	response := resmgrsvc.SetPlacementsResponse{}
	h.metrics.PlacementQueueLen.Update(float64(h.placements.Length()))
	log.Debug("Set Placement Returned")
	return &response, nil
}

// GetTasksByHosts returns all tasks of the given task type running on the given list of hosts.
func (h *ServiceHandler) GetTasksByHosts(ctx context.Context,
	req *resmgrsvc.GetTasksByHostsRequest) (*resmgrsvc.GetTasksByHostsResponse, error) {
	hostTasksMap := map[string]*resmgrsvc.TaskList{}
	for hostname, tasks := range h.rmTracker.TasksByHosts(req.Hostnames, req.Type) {
		if _, exists := hostTasksMap[hostname]; !exists {
			hostTasksMap[hostname] = &resmgrsvc.TaskList{
				Tasks: make([]*resmgr.Task, 0, len(tasks)),
			}
		}
		for _, task := range tasks {
			hostTasksMap[hostname].Tasks = append(hostTasksMap[hostname].Tasks, task.Task())
		}
	}
	res := &resmgrsvc.GetTasksByHostsResponse{
		HostTasksMap: hostTasksMap,
	}
	return res, nil
}

func (h *ServiceHandler) removeTasksFromPlacements(
	placement *resmgr.Placement,
	tasks map[string]*peloton.TaskID,
) *resmgr.Placement {
	if len(tasks) == 0 {
		return placement
	}
	var newTasks []*peloton.TaskID

	log.WithFields(log.Fields{
		"Removed Tasks":  tasks,
		"Original Tasks": placement.GetTasks(),
	}).Debug("Removing Tasks")

	for _, pt := range placement.GetTasks() {
		if _, ok := tasks[pt.Value]; !ok {
			newTasks = append(newTasks, pt)
		}
	}
	placement.Tasks = newTasks
	return placement
}

// GetPlacements implements ResourceManagerService.GetPlacements
func (h *ServiceHandler) GetPlacements(
	ctx context.Context,
	req *resmgrsvc.GetPlacementsRequest,
) (*resmgrsvc.GetPlacementsResponse, error) {

	log.WithField("request", req).Debug("GetPlacements called.")
	h.metrics.APIGetPlacements.Inc(1)

	limit := req.GetLimit()
	timeout := time.Duration(req.GetTimeout())

	h.metrics.APIGetPlacements.Inc(1)
	var placements []*resmgr.Placement
	for i := 0; i < int(limit); i++ {
		item, err := h.placements.Dequeue(timeout * time.Millisecond)

		if err != nil {
			h.metrics.GetPlacementFail.Inc(1)
			break
		}
		placement := item.(*resmgr.Placement)
		newPlacement := h.transitTasksInPlacement(placement,
			t.TaskState_PLACED,
			t.TaskState_LAUNCHING)
		placements = append(placements, newPlacement)
		h.metrics.GetPlacementSuccess.Inc(1)
	}

	response := resmgrsvc.GetPlacementsResponse{Placements: placements}
	h.metrics.PlacementQueueLen.Update(float64(h.placements.Length()))
	log.Debug("Get Placement Returned")

	return &response, nil
}

// transitTasksInPlacement transition to Launching upon getplacement
// or remove tasks from placement which are not in placed state.
func (h *ServiceHandler) transitTasksInPlacement(
	placement *resmgr.Placement,
	oldState t.TaskState,
	newState t.TaskState) *resmgr.Placement {
	invalidTasks := make(map[string]*peloton.TaskID)
	for _, taskID := range placement.Tasks {
		rmTask := h.rmTracker.GetTask(taskID)
		if rmTask == nil {
			invalidTasks[taskID.Value] = taskID
			log.WithFields(log.Fields{
				"Task": taskID.Value,
			}).Debug("Task is not present in tracker, " +
				"Removing it from placement")
			continue
		}
		state := rmTask.GetCurrentState()
		log.WithFields(log.Fields{
			"Current state": state.String(),
			"Task":          taskID.Value,
		}).Debug("Get Placement for task")
		if state != oldState {
			log.WithField("task_id", taskID.GetValue()).
				Error("Task is not in placed state")
			invalidTasks[taskID.Value] = taskID

		} else {
			err := rmTask.TransitTo(newState.String())
			if err != nil {
				log.WithError(errors.WithStack(err)).
					WithField("task_id", taskID.GetValue()).
					Info("not able to transition to launching for task")
				invalidTasks[taskID.Value] = taskID
			}
		}
		log.WithFields(log.Fields{
			"Task":  taskID.Value,
			"State": state.String(),
		}).Debug("Latest state in Get Placement")
	}
	return h.removeTasksFromPlacements(placement, invalidTasks)
}

// NotifyTaskUpdates is called by HM to notify task updates
func (h *ServiceHandler) NotifyTaskUpdates(
	ctx context.Context,
	req *resmgrsvc.NotifyTaskUpdatesRequest) (*resmgrsvc.NotifyTaskUpdatesResponse, error) {
	var response resmgrsvc.NotifyTaskUpdatesResponse

	if len(req.Events) == 0 {
		log.Warn("Empty events received by resource manager")
		return &response, nil
	}

	for _, event := range req.Events {
		taskState := util.MesosStateToPelotonState(
			event.MesosTaskStatus.GetState())
		if taskState != t.TaskState_RUNNING &&
			!util.IsPelotonStateTerminal(taskState) {
			h.acknowledgeEvent(event.Offset)
			continue
		}
		ptID, err := util.ParseTaskIDFromMesosTaskID(
			*(event.MesosTaskStatus.TaskId.Value))
		if err != nil {
			log.WithField("event", event).Error("Could not parse mesos ID")
			h.acknowledgeEvent(event.Offset)
			continue
		}
		taskID := &peloton.TaskID{
			Value: ptID,
		}
		rmTask := h.rmTracker.GetTask(taskID)
		if rmTask == nil {
			h.acknowledgeEvent(event.Offset)
			continue
		}

		if *(rmTask.Task().TaskId.Value) !=
			*(event.MesosTaskStatus.TaskId.Value) {
			log.WithFields(log.Fields{
				"event": event,
				"Task":  rmTask.Task().Id,
			}).Error("could not be updated due to" +
				"different mesos taskID")
			h.acknowledgeEvent(event.Offset)
			continue
		}
		if taskState == t.TaskState_RUNNING {
			err = rmTask.TransitTo(t.TaskState_RUNNING.String())
			if err != nil {
				log.WithError(errors.WithStack(err)).
					WithField("task_id", taskID.Value).
					Info("Not able to transition to RUNNING for task")
			}
		} else {
			// TODO: We probably want to terminate all the tasks in gang
			err = rmtask.GetTracker().MarkItDone(taskID)
			if err != nil {
				log.WithField("event", event).Error("Could not be updated")
			}
			rmtask.GetTracker().UpdateCounters(
				t.TaskState_RUNNING.String(), taskState.String())
		}
		h.acknowledgeEvent(event.Offset)
	}
	response.PurgeOffset = atomic.LoadUint64(h.maxOffset)
	return &response, nil
}

func (h *ServiceHandler) acknowledgeEvent(offset uint64) {
	log.WithField("Offset", offset).
		Debug("Event received by resource manager")
	if offset > atomic.LoadUint64(h.maxOffset) {
		atomic.StoreUint64(h.maxOffset, offset)
	}
}

// GetActiveTasks returns task to state map
func (h *ServiceHandler) GetActiveTasks(
	ctx context.Context,
	req *resmgrsvc.GetActiveTasksRequest,
) (*resmgrsvc.GetActiveTasksResponse, error) {
	taskStates := h.rmTracker.GetActiveTasks(req.GetJobID(), req.GetRespoolID())
	return &resmgrsvc.GetActiveTasksResponse{TaskStatesMap: taskStates}, nil
}

// KillTasks kills the task
func (h *ServiceHandler) KillTasks(
	ctx context.Context,
	req *resmgrsvc.KillTasksRequest,
) (*resmgrsvc.KillTasksResponse, error) {
	listTasks := req.GetTasks()
	if len(listTasks) == 0 {
		return &resmgrsvc.KillTasksResponse{
			Error: &resmgrsvc.KillTasksResponse_Error{
				Message: "Killed tasks called with no tasks",
			},
		}, nil
	}
	var tasksNotKilled string
	for _, killedTask := range listTasks {
		killedRmTask := h.rmTracker.GetTask(killedTask)

		if killedRmTask == nil {
			tasksNotKilled += killedTask.Value + " , "
			continue
		}

		err := h.rmTracker.MarkItDone(killedTask)
		if err != nil {
			tasksNotKilled += killedTask.Value + " , "
		}
		h.rmTracker.UpdateCounters(
			killedRmTask.GetCurrentState().String(),
			t.TaskState_KILLED.String(),
		)
	}
	if tasksNotKilled == "" {
		return &resmgrsvc.KillTasksResponse{}, nil

	}
	log.WithField("Tasks", tasksNotKilled).Error("tasks can't be killed")
	return &resmgrsvc.KillTasksResponse{
		Error: &resmgrsvc.KillTasksResponse_Error{
			Message: "tasks can't be killed " +
				tasksNotKilled,
		},
	}, nil
}
