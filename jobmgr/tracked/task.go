package tracked

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	pb_task "code.uber.internal/infra/peloton/.gen/peloton/api/task"
)

const (
	// UnknownVersion is used by the goalstate engine, when either the current
	// or desired config version is unknown.
	UnknownVersion = math.MaxUint64
)

// Task tracked by the system, serving as a best effort view of what's stored
// in the database.
type Task interface {
	// ID of the task.
	ID() uint32

	// Job the task belongs to.
	Job() Job

	// CurrentState of the task.
	CurrentState() State

	// GoalState of the task.
	GoalState() GoalState

	// LastAction performed by the task, as well as when it was performed.
	LastAction() (TaskAction, time.Time)

	// RunAction on the task.
	RunAction(ctx context.Context, action TaskAction) error
}

// State of a task. This encapsulate the actual state.
type State struct {
	State         pb_task.TaskState
	ConfigVersion uint64
}

// GoalState of a task. This encapsulate the goal state.
type GoalState struct {
	State         pb_task.TaskGoalState
	ConfigVersion uint64
}

// TaskAction that can be given to the Task.RunAction method.
type TaskAction string

// Actions available to be performed on the task.
const (
	NoAction             TaskAction = "no_action"
	UntrackAction        TaskAction = "untrack"
	InitializeAction     TaskAction = "initialize_task"
	StartAction          TaskAction = "start_task"
	StopAction           TaskAction = "stop_task"
	UseGoalVersionAction TaskAction = "use_goal_state"
)

func newTask(job *job, id uint32) *task {
	task := &task{
		queueItemMixin: newQueueItemMixing(),
		job:            job,
		id:             id,
	}

	return task
}

// task is the wrapper around task info for state machine
type task struct {
	sync.RWMutex
	queueItemMixin

	job *job
	id  uint32

	runtime *pb_task.RuntimeInfo

	// goalState along with the time the goal state was updated.
	stateTime     time.Time
	goalStateTime time.Time

	// lastState set, the resulting action and when that action was last tried.
	lastAction     TaskAction
	lastActionTime time.Time
}

func (t *task) ID() uint32 {
	return t.id
}

func (t *task) Job() Job {
	return t.job
}

func (t *task) CurrentState() State {
	t.RLock()
	defer t.RUnlock()

	return State{
		State:         t.runtime.GetState(),
		ConfigVersion: t.runtime.GetConfigVersion(),
	}
}

func (t *task) GoalState() GoalState {
	t.RLock()
	defer t.RUnlock()

	return GoalState{
		State:         t.runtime.GetGoalState(),
		ConfigVersion: t.runtime.GetDesiredConfigVersion(),
	}
}

func (t *task) LastAction() (TaskAction, time.Time) {
	t.RLock()
	defer t.RUnlock()

	return t.lastAction, t.lastActionTime
}

func (t *task) RunAction(ctx context.Context, action TaskAction) error {
	defer t.job.m.mtx.scope.Tagged(map[string]string{"action": string(action)}).Timer("run_duration").Start().Stop()

	// TODO: Move to Manager, such that the following holds:
	// Take job lock only while we evaluate action. That ensure we have a
	// consistent view across the entire job, while we decide if we can apply
	// the action.

	t.Lock()
	t.lastAction = action
	t.lastActionTime = time.Now()
	t.Unlock()

	log.WithField("action", action).
		WithField("current_state", t.CurrentState().State.String()).
		WithField("current_config", t.CurrentState().ConfigVersion).
		WithField("goal_state", t.GoalState().State.String()).
		WithField("goal_version", t.GoalState().ConfigVersion).
		WithField("job_id", t.job.id.GetValue()).
		WithField("instance_id", t.id).
		Info("running action for task")

	var err error
	switch action {
	case NoAction:

	case UntrackAction:
		t.job.m.clearTask(t)

	case InitializeAction:
		err = t.initialize(ctx)

	case StartAction:
		err = t.start(ctx)

	case StopAction:
		err = t.stop(ctx)

	default:
		err = fmt.Errorf("no command configured for running task action `%v`", action)
	}

	return err
}

// getRuntime returns a shallow copy of the runtime, or an error if's not set.
func (t *task) getRuntime() (*pb_task.RuntimeInfo, error) {
	t.RLock()
	defer t.RUnlock()

	if t.runtime == nil {
		return nil, fmt.Errorf("tracked task has no runtime info assigned")
	}

	// Shallow copy of the runtime.
	runtime := *t.runtime
	// Also do a copy of the revision, to ensure storage doesn't change it.
	if runtime.Revision != nil {
		revision := *runtime.Revision
		runtime.Revision = &revision
	}
	return &runtime, nil
}

func (t *task) updateRuntime(runtime *pb_task.RuntimeInfo) {
	t.Lock()
	defer t.Unlock()

	// Ignore older revisions of the task runtime.
	if runtime.GetRevision().GetVersion() < t.runtime.GetRevision().GetVersion() {
		return
	}

	t.runtime = runtime

	now := time.Now()
	t.goalStateTime = now
	t.stateTime = now
}
