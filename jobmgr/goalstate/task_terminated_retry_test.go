package goalstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	mesosv1 "code.uber.internal/infra/peloton/.gen/mesos/v1"
	pbjob "code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	pbtask "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	pbupdate "code.uber.internal/infra/peloton/.gen/peloton/api/v0/update"
	"code.uber.internal/infra/peloton/.gen/peloton/private/models"
	"code.uber.internal/infra/peloton/jobmgr/cached"

	"code.uber.internal/infra/peloton/common/goalstate"
	goalstatemocks "code.uber.internal/infra/peloton/common/goalstate/mocks"
	cachedmocks "code.uber.internal/infra/peloton/jobmgr/cached/mocks"
	jobmgrcommon "code.uber.internal/infra/peloton/jobmgr/common"
	storemocks "code.uber.internal/infra/peloton/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

type TaskTerminatedRetryTestSuite struct {
	suite.Suite
	ctrl *gomock.Controller

	taskStore  *storemocks.MockTaskStore
	jobFactory *cachedmocks.MockJobFactory

	updateGoalStateEngine *goalstatemocks.MockEngine
	taskGoalStateEngine   *goalstatemocks.MockEngine
	jobGoalStateEngine    *goalstatemocks.MockEngine
	goalStateDriver       *driver

	jobID      *peloton.JobID
	instanceID uint32
	updateID   *peloton.UpdateID

	taskEnt      *taskEntity
	cachedJob    *cachedmocks.MockJob
	cachedUpdate *cachedmocks.MockUpdate
	cachedTask   *cachedmocks.MockTask

	jobRuntime  *pbjob.RuntimeInfo
	taskConfig  *pbtask.TaskConfig
	taskRuntime *pbtask.RuntimeInfo
	mesosTaskID string
}

func TestTerminatedFailRetryStart(t *testing.T) {
	suite.Run(t, new(TaskTerminatedRetryTestSuite))
}

func (suite *TaskTerminatedRetryTestSuite) TearDownTest() {
	suite.ctrl.Finish()
}

func (suite *TaskTerminatedRetryTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.taskStore = storemocks.NewMockTaskStore(suite.ctrl)
	suite.jobGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.taskGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.updateGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.jobFactory = cachedmocks.NewMockJobFactory(suite.ctrl)
	suite.cachedJob = cachedmocks.NewMockJob(suite.ctrl)
	suite.cachedTask = cachedmocks.NewMockTask(suite.ctrl)
	suite.cachedUpdate = cachedmocks.NewMockUpdate(suite.ctrl)
	suite.goalStateDriver = &driver{
		jobEngine:    suite.jobGoalStateEngine,
		taskEngine:   suite.taskGoalStateEngine,
		updateEngine: suite.updateGoalStateEngine,
		taskStore:    suite.taskStore,
		jobFactory:   suite.jobFactory,
		mtx:          NewMetrics(tally.NoopScope),
		cfg: &Config{
			InitialTaskBackoff: 30 * time.Second,
			MaxTaskBackoff:     60 * time.Minute,
		},
	}
	suite.goalStateDriver.cfg.normalize()
	suite.jobID = &peloton.JobID{Value: uuid.NewRandom().String()}
	suite.updateID = &peloton.UpdateID{Value: uuid.NewRandom().String()}
	suite.instanceID = uint32(0)
	suite.taskEnt = &taskEntity{
		jobID:      suite.jobID,
		instanceID: suite.instanceID,
		driver:     suite.goalStateDriver,
	}
	suite.mesosTaskID = fmt.Sprintf("%s-%d-%d", suite.jobID.GetValue(), suite.instanceID, 1)
	suite.taskRuntime = &pbtask.RuntimeInfo{
		MesosTaskId:   &mesosv1.TaskID{Value: &suite.mesosTaskID},
		State:         pbtask.TaskState_FAILED,
		GoalState:     pbtask.TaskState_SUCCEEDED,
		ConfigVersion: 1,
		Message:       "testFailure",
		Reason:        mesosv1.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED.String(),
		FailureCount:  5,
	}
	suite.jobRuntime = &pbjob.RuntimeInfo{
		UpdateID: suite.updateID,
	}
	suite.taskConfig = &pbtask.TaskConfig{}
}

// TestTaskFailRetryStatelessNoTask tests restart when the job is not in cache
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoJob() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(nil)

	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}

//TestTaskTerminatedRetryNoJobRuntime tests restart when the job has no runtime
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoJobRuntime() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(nil, fmt.Errorf(""))
	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}

// TestTaskFailRetryStatelessNoTask tests restart when the task is not in cache
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoTask() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(nil, fmt.Errorf("fake db error"))
	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}

// TestTaskFailRetryStatelessNoTask tests restart when no task runtime
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoTaskRuntime() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(nil, fmt.Errorf(""))
	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}

// TestTaskFailRetryStatelessNoTask tests restart when no task config
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoTaskConfig() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)
	suite.taskStore.EXPECT().GetTaskConfig(
		gomock.Any(),
		suite.jobID,
		suite.instanceID,
		gomock.Any()).Return(nil, nil, fmt.Errorf(""))

	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}

// TestTaskFailRetryStatelessNoTask tests restart when no update
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoUpdate() {
	jobRuntime := &pbjob.RuntimeInfo{}

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.taskRuntime.FailureCount = 2
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)
	suite.taskConfig = &pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 3,
		},
	}
	suite.taskStore.EXPECT().GetTaskConfig(
		gomock.Any(),
		suite.jobID,
		suite.instanceID,
		gomock.Any()).Return(suite.taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		ID().Return(suite.jobID)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			runtimeDiff := runtimeDiffs[suite.instanceID]
			suite.True(
				runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetJobType().Return(pbjob.JobType_BATCH)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Do(func(_ goalstate.Entity, deadline time.Time) {
			suite.True(deadline.After(time.Now()))
		}).
		Return()

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}

// TestTaskTerminatedRetryNoFailure tests restart when there is no failure
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryNoFailure() {
	updateConfig := pbupdate.UpdateConfig{
		MaxInstanceAttempts: uint32(3),
	}

	suite.taskRuntime.FailureCount = 0

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)
	suite.taskStore.EXPECT().GetTaskConfig(
		gomock.Any(),
		suite.jobID,
		suite.instanceID,
		gomock.Any()).Return(suite.taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		AddWorkflow(suite.updateID).
		Return(suite.cachedUpdate)
	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		}).
		Times(2)
	suite.cachedUpdate.EXPECT().IsTaskInUpdateProgress(
		suite.instanceID).Return(true)
	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().Return(&updateConfig)

	suite.cachedJob.EXPECT().
		ID().Return(suite.jobID)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			runtimeDiff := runtimeDiffs[suite.instanceID]
			suite.True(
				runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetJobType().Return(pbjob.JobType_BATCH)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}

// TestTaskTooManyFailuresNoRetry tests not retry on a task with too many failures
func (suite *TaskTerminatedRetryTestSuite) TestTaskTooManyFailuresNoRetry() {
	updateConfig := pbupdate.UpdateConfig{
		MaxInstanceAttempts: uint32(3),
	}
	suite.taskRuntime.FailureCount = 5

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)
	suite.taskStore.EXPECT().GetTaskConfig(
		gomock.Any(),
		suite.jobID,
		suite.instanceID,
		gomock.Any()).Return(suite.taskConfig, &models.ConfigAddOn{}, nil)
	suite.cachedJob.EXPECT().
		AddWorkflow(suite.updateID).
		Return(suite.cachedUpdate)
	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		}).
		Times(2)
	suite.cachedUpdate.EXPECT().IsTaskInUpdateProgress(
		suite.instanceID).Return(true)
	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().Return(&updateConfig)
	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)
	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}

// TestTaskTerminatedRetryUpdateTerminated tests restart when update is terminal
func (suite *TaskTerminatedRetryTestSuite) TestTaskTerminatedRetryUpdateTerminated() {
	suite.taskRuntime.FailureCount = 5

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)
	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).Return(suite.jobRuntime, nil)
	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), suite.instanceID).Return(suite.cachedTask, nil)
	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)
	suite.taskStore.EXPECT().GetTaskConfig(
		gomock.Any(),
		suite.jobID,
		suite.instanceID,
		gomock.Any()).Return(suite.taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		AddWorkflow(suite.updateID).
		Return(suite.cachedUpdate)
	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_SUCCEEDED,
		}).
		Times(2)
	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)
	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)
	suite.updateGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()
	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)
	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			runtimeDiff := runtimeDiffs[suite.instanceID]
			suite.True(
				runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetJobType().Return(pbjob.JobType_BATCH)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := TaskTerminatedRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}