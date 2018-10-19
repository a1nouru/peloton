package hostmgr

import (
	"code.uber.internal/infra/peloton/hostmgr/host"
	"code.uber.internal/infra/peloton/hostmgr/metrics"
	"code.uber.internal/infra/peloton/hostmgr/queue"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"

	hpb "code.uber.internal/infra/peloton/.gen/peloton/api/v0/host"

	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"
)

// RecoveryHandler defines the interface to
// be called by leader election callbacks.
type RecoveryHandler interface {
	Start() error
	Stop() error
}

// recoveryHandler restores the contents of MaintenanceQueue
// from Mesos Maintenance Status
type recoveryHandler struct {
	metrics                *metrics.Metrics
	maintenanceQueue       queue.MaintenanceQueue
	masterOperatorClient   mpb.MasterOperatorClient
	maintenanceHostInfoMap host.MaintenanceHostInfoMap
}

// NewRecoveryHandler creates a recoveryHandler
func NewRecoveryHandler(parent tally.Scope,
	maintenanceQueue queue.MaintenanceQueue,
	masterOperatorClient mpb.MasterOperatorClient,
	maintenanceHostInfoMap host.MaintenanceHostInfoMap) RecoveryHandler {
	recovery := &recoveryHandler{
		metrics:                metrics.NewMetrics(parent),
		maintenanceQueue:       maintenanceQueue,
		masterOperatorClient:   masterOperatorClient,
		maintenanceHostInfoMap: maintenanceHostInfoMap,
	}
	return recovery
}

// Stop is a no-op for recovery handler
func (r *recoveryHandler) Stop() error {
	log.Info("Stopping recovery")
	return nil
}

// Start requeues all 'DRAINING' hosts into maintenance queue
func (r *recoveryHandler) Start() error {
	err := r.recoverMaintenanceState()
	if err != nil {
		r.metrics.RecoveryFail.Inc(1)
		return err
	}

	r.metrics.RecoverySuccess.Inc(1)
	return nil
}

func (r *recoveryHandler) recoverMaintenanceState() error {
	// Clear contents of maintenance queue before
	// enqueuing, to ensure removal of stale data
	r.maintenanceQueue.Clear()

	response, err := r.masterOperatorClient.GetMaintenanceStatus()
	if err != nil {
		return err
	}

	clusterStatus := response.GetStatus()
	if clusterStatus == nil {
		log.Info("Empty maintenance status received")
		return nil
	}

	var drainingHosts []string
	var hostInfos []*hpb.HostInfo
	for _, drainingMachine := range clusterStatus.GetDrainingMachines() {
		machineID := drainingMachine.GetId()
		hostInfos = append(hostInfos,
			&hpb.HostInfo{
				Hostname: machineID.GetHostname(),
				Ip:       machineID.GetIp(),
				State:    hpb.HostState_HOST_STATE_DRAINING,
			})
		drainingHosts = append(drainingHosts, machineID.GetHostname())
	}

	for _, downMachine := range clusterStatus.GetDownMachines() {
		hostInfos = append(hostInfos,
			&hpb.HostInfo{
				Hostname: downMachine.GetHostname(),
				Ip:       downMachine.GetIp(),
				State:    hpb.HostState_HOST_STATE_DOWN,
			})
	}
	r.maintenanceHostInfoMap.AddHostInfos(hostInfos)
	return r.maintenanceQueue.Enqueue(drainingHosts)
}
