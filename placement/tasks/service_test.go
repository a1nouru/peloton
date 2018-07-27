package tasks

import (
	"context"
	"errors"
	"testing"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	resource_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"
	"code.uber.internal/infra/peloton/placement/config"
	"code.uber.internal/infra/peloton/placement/metrics"
	"code.uber.internal/infra/peloton/placement/models"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"
)

const (
	_testReason = "Test Placement Reason"
)

func setupService(t *testing.T) (Service, *resource_mocks.MockResourceManagerServiceYARPCClient, *gomock.Controller) {
	ctrl := gomock.NewController(t)
	mockResourceManager := resource_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	metrics := metrics.NewMetrics(tally.NoopScope)
	config := &config.PlacementConfig{
		MaxRounds: config.MaxRoundsConfig{
			Unknown:   1,
			Batch:     1,
			Stateless: 5,
			Daemon:    5,
			Stateful:  0,
		},
		MaxDurations: config.MaxDurationsConfig{
			Unknown:   5 * time.Second,
			Batch:     5 * time.Second,
			Stateless: 10 * time.Second,
			Daemon:    15 * time.Second,
			Stateful:  25 * time.Second,
		},
	}
	return NewService(mockResourceManager, config, metrics), mockResourceManager, ctrl
}

func TestTaskService_Dequeue(t *testing.T) {
	service, mockResourceManager, ctrl := setupService(t)
	defer ctrl.Finish()
	ctx := context.Background()

	request := &resmgrsvc.DequeueGangsRequest{
		Limit:   10,
		Type:    resmgr.TaskType_UNKNOWN,
		Timeout: 100,
	}

	// Placement engine dequeue, resource manager dequeue gangs api failure
	response := &resmgrsvc.DequeueGangsResponse{
		Error: &resmgrsvc.DequeueGangsResponse_Error{},
	}
	mockResourceManager.EXPECT().
		DequeueGangs(
			gomock.Any(),
			request,
		).Return(
		response,
		nil,
	)
	assignments := service.Dequeue(ctx, resmgr.TaskType_UNKNOWN, 10, 100)
	assert.Nil(t, assignments)

	mockResourceManager.EXPECT().
		DequeueGangs(
			gomock.Any(),
			request,
		).Return(
		nil,
		errors.New("dequeue gangs request failed"),
	)
	assignments = service.Dequeue(ctx, resmgr.TaskType_UNKNOWN, 10, 100)
	assert.Nil(t, assignments)

	// Placement engine dequeue gangs with nil task
	gomock.InOrder(
		mockResourceManager.EXPECT().
			DequeueGangs(
				gomock.Any(),
				request,
			).Return(
			&resmgrsvc.DequeueGangsResponse{
				Gangs: []*resmgrsvc.Gang{
					{
						Tasks: nil,
					},
				},
			},
			nil,
		),
	)

	assignments = service.Dequeue(ctx, resmgr.TaskType_UNKNOWN, 10, 100)
	assert.Nil(t, assignments)

	// Placement engine dequeue success call
	gomock.InOrder(
		mockResourceManager.EXPECT().
			DequeueGangs(
				gomock.Any(),
				request,
			).Return(
			&resmgrsvc.DequeueGangsResponse{
				Gangs: []*resmgrsvc.Gang{
					{
						Tasks: []*resmgr.Task{
							{
								Name: "task",
							},
						},
					},
				},
			},
			nil,
		),
	)

	assignments = service.Dequeue(ctx, resmgr.TaskType_UNKNOWN, 10, 100)
	assert.NotNil(t, assignments)
	assert.Equal(t, 1, len(assignments))
}

func TestTaskService_SetPlacements(t *testing.T) {
	service, mockResourceManager, ctrl := setupService(t)
	defer ctrl.Finish()

	ctx := context.Background()
	placements := []*resmgr.Placement{
		{
			Hostname: "hostname",
			Tasks: []*peloton.TaskID{
				{
					Value: "taskid",
				},
			},
		},
	}
	request := &resmgrsvc.SetPlacementsRequest{
		Placements: placements,
	}

	// Placement engine with empty placements
	service.SetPlacements(ctx, nil)

	// Placement engine, resource manager set placements request failed
	mockResourceManager.EXPECT().
		SetPlacements(
			gomock.Any(),
			request,
		).
		Return(
			&resmgrsvc.SetPlacementsResponse{
				Error: &resmgrsvc.SetPlacementsResponse_Error{},
			},
			nil,
		)
	service.SetPlacements(ctx, placements)

	mockResourceManager.EXPECT().
		SetPlacements(
			gomock.Any(),
			request,
		).
		Return(
			&resmgrsvc.SetPlacementsResponse{
				Error: &resmgrsvc.SetPlacementsResponse_Error{
					Failure: &resmgrsvc.SetPlacementsFailure{},
				},
			},
			nil,
		)
	service.SetPlacements(ctx, placements)

	mockResourceManager.EXPECT().
		SetPlacements(
			gomock.Any(),
			request,
		).
		Return(
			nil,
			errors.New("resource manager set placements request failed"),
		)
	service.SetPlacements(ctx, placements)

	gomock.InOrder(
		mockResourceManager.EXPECT().
			SetPlacements(
				gomock.Any(),
				&resmgrsvc.SetPlacementsRequest{
					Placements: placements,
				},
			).
			Return(
				&resmgrsvc.SetPlacementsResponse{},
				nil,
			),
	)
	service.SetPlacements(ctx, placements)
}

func TestTaskService_Enqueue(t *testing.T) {
	service, mockResourceManager, ctrl := setupService(t)
	defer ctrl.Finish()
	ctx := context.Background()
	request := &resmgrsvc.EnqueueGangsRequest{
		Gangs: []*resmgrsvc.Gang{
			{
				Tasks: []*resmgr.Task{
					{
						Name: "task",
					},
				},
			},
		},
		Reason: _testReason,
	}
	rmTask := &resmgr.Task{
		Name: "task",
	}
	gang := &resmgrsvc.Gang{
		Tasks: []*resmgr.Task{rmTask},
	}
	task := models.NewTask(gang, rmTask, time.Now().Add(10*time.Second), 1)
	assignments := []*models.Assignment{models.NewAssignment(task)}

	// Placement engine enqueue nil assignments
	service.Enqueue(ctx, nil, _testReason)

	// Placement engine enqueue resmgr enqueue gangs request errors
	mockResourceManager.EXPECT().EnqueueGangs(
		gomock.Any(),
		request,
	).Return(nil, errors.New("enqueue gangs request failed"))
	service.Enqueue(ctx, assignments, _testReason)

	response := &resmgrsvc.EnqueueGangsResponse{
		Error: &resmgrsvc.EnqueueGangsResponse_Error{},
	}
	mockResourceManager.EXPECT().EnqueueGangs(
		gomock.Any(),
		request,
	).Return(response, nil)
	service.Enqueue(ctx, assignments, _testReason)

	// Placement engine enqueue success case
	gomock.InOrder(
		mockResourceManager.EXPECT().EnqueueGangs(
			gomock.Any(),
			request,
		),
	)
	service.Enqueue(ctx, assignments, _testReason)
}
