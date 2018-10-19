package host

import (
	"errors"
	"fmt"
	"testing"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	mesos_master "code.uber.internal/infra/peloton/.gen/mesos/v1/master"
	host "code.uber.internal/infra/peloton/.gen/peloton/api/v0/host"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/util"
	mock_mpb "code.uber.internal/infra/peloton/yarpc/encoding/mpb/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

var (
	_defaultResourceValue = 1
)

type HostMapTestSuite struct {
	suite.Suite

	ctrl           *gomock.Controller
	testScope      tally.TestScope
	operatorClient *mock_mpb.MockMasterOperatorClient
}

func (suite *HostMapTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.testScope = tally.NewTestScope("", map[string]string{})
	suite.operatorClient = mock_mpb.NewMockMasterOperatorClient(suite.ctrl)
}

func makeAgentsResponse(numAgents int) *mesos_master.Response_GetAgents {
	response := &mesos_master.Response_GetAgents{
		Agents: []*mesos_master.Response_GetAgents_Agent{},
	}
	for i := 0; i < numAgents; i++ {
		resVal := float64(_defaultResourceValue)
		tmpID := fmt.Sprintf("id-%d", i)
		resources := []*mesos.Resource{
			util.NewMesosResourceBuilder().
				WithName(common.MesosCPU).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(common.MesosMem).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(common.MesosDisk).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(common.MesosGPU).
				WithValue(resVal).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(common.MesosCPU).
				WithValue(resVal).
				WithRevocable(&mesos.Resource_RevocableInfo{}).
				Build(),
			util.NewMesosResourceBuilder().
				WithName(common.MesosMem).
				WithValue(resVal).
				WithRevocable(&mesos.Resource_RevocableInfo{}).
				Build(),
		}
		getAgent := &mesos_master.Response_GetAgents_Agent{
			AgentInfo: &mesos.AgentInfo{
				Hostname:  &tmpID,
				Resources: resources,
			},
			TotalResources: resources,
		}
		response.Agents = append(response.Agents, getAgent)
	}

	return response
}

func (suite *HostMapTestSuite) TestRefresh() {
	defer suite.ctrl.Finish()

	loader := &Loader{
		OperatorClient:     suite.operatorClient,
		Scope:              suite.testScope,
		SlackResourceTypes: []string{common.MesosCPU},
	}

	gomock.InOrder(
		suite.operatorClient.EXPECT().Agents().
			Return(nil, errors.New("unable to get agents")),
	)
	loader.Load(nil)

	numAgents := 2000
	response := makeAgentsResponse(numAgents)
	gomock.InOrder(
		suite.operatorClient.EXPECT().Agents().Return(response, nil),
	)
	loader.Load(nil)

	m := GetAgentMap()
	suite.Len(m.RegisteredAgents, numAgents)

	id1 := "id-1"
	a1 := GetAgentInfo(id1)
	suite.NotEmpty(a1.Resources)
	id2 := "id-20000"
	a2 := GetAgentInfo(id2)
	suite.Nil(a2)

	gauges := suite.testScope.Snapshot().Gauges()
	suite.Contains(gauges, "registered_hosts+")
	suite.Equal(float64(numAgents), gauges["registered_hosts+"].Value())
	suite.Contains(gauges, "cpus+")
	suite.Equal(float64(numAgents*_defaultResourceValue), gauges["cpus+"].Value())
	suite.Contains(gauges, "cpus_revocable+")
	suite.Equal(float64(numAgents*_defaultResourceValue), gauges["cpus_revocable+"].Value())
	suite.Contains(gauges, "mem+")
	suite.Equal(float64(numAgents*_defaultResourceValue), gauges["mem+"].Value())
	suite.Contains(gauges, "disk+")
	suite.Equal(float64(numAgents*_defaultResourceValue), gauges["disk+"].Value())
	suite.Contains(gauges, "gpus+")
	suite.Equal(float64(numAgents*_defaultResourceValue), gauges["gpus+"].Value())
}

func (suite *HostMapTestSuite) TestMaintenanceHostInfoMap() {
	maintenanceHostInfoMap := NewMaintenanceHostInfoMap()
	suite.NotNil(maintenanceHostInfoMap)

	drainingHostInfos := []*host.HostInfo{
		{
			Hostname: "host1",
			Ip:       "0.0.0.0",
			State:    host.HostState_HOST_STATE_DRAINING,
		},
	}

	downHostInfos := []*host.HostInfo{
		{
			Hostname: "host2",
			Ip:       "0.0.0.1",
			State:    host.HostState_HOST_STATE_DOWN,
		},
	}

	var (
		drainingHosts []string
		downHosts     []string
	)

	for _, hostInfo := range drainingHostInfos {
		drainingHosts = append(drainingHosts, hostInfo.GetHostname())
	}
	for _, hostInfo := range downHostInfos {
		downHosts = append(downHosts, hostInfo.GetHostname())
	}
	suite.Nil(maintenanceHostInfoMap.GetDrainingHostInfos([]string{}))
	maintenanceHostInfoMap.AddHostInfos(drainingHostInfos)
	suite.NotEmpty(maintenanceHostInfoMap.GetDrainingHostInfos(drainingHosts))

	suite.Nil(maintenanceHostInfoMap.GetDownHostInfos([]string{}))
	maintenanceHostInfoMap.AddHostInfos(downHostInfos)
	suite.NotEmpty(maintenanceHostInfoMap.GetDownHostInfos(downHosts))

	drainingHostInfoMap := make(map[string]*host.HostInfo)
	for _, hostInfo := range maintenanceHostInfoMap.GetDrainingHostInfos([]string{}) {
		drainingHostInfoMap[hostInfo.GetHostname()] = hostInfo
	}
	for _, hostInfo := range drainingHostInfos {
		suite.NotNil(drainingHostInfoMap[hostInfo.GetHostname()])
		suite.Equal(hostInfo.GetHostname(),
			drainingHostInfoMap[hostInfo.GetHostname()].GetHostname())
		suite.Equal(hostInfo.GetIp(),
			drainingHostInfoMap[hostInfo.GetHostname()].GetIp())
		suite.Equal(host.HostState_HOST_STATE_DRAINING,
			drainingHostInfoMap[hostInfo.GetHostname()].GetState())
	}

	downHostInfoMap := make(map[string]*host.HostInfo)
	for _, hostInfo := range maintenanceHostInfoMap.GetDownHostInfos([]string{}) {
		downHostInfoMap[hostInfo.GetHostname()] = hostInfo
	}
	for _, hostInfo := range downHostInfos {
		suite.NotNil(downHostInfoMap[hostInfo.GetHostname()])
		suite.Equal(hostInfo.GetHostname(),
			downHostInfoMap[hostInfo.GetHostname()].GetHostname())
		suite.Equal(hostInfo.GetIp(),
			downHostInfoMap[hostInfo.GetHostname()].GetIp())
		suite.Equal(host.HostState_HOST_STATE_DOWN,
			downHostInfoMap[hostInfo.GetHostname()].GetState())
	}

	// Test UpdateHostState
	err := maintenanceHostInfoMap.UpdateHostState(
		drainingHosts[0],
		host.HostState_HOST_STATE_DRAINING,
		host.HostState_HOST_STATE_DOWN)
	suite.NoError(err)
	suite.Empty(
		maintenanceHostInfoMap.GetDrainingHostInfos([]string{drainingHosts[0]}))
	suite.NotEmpty(
		maintenanceHostInfoMap.GetDownHostInfos([]string{drainingHosts[0]}))

	err = maintenanceHostInfoMap.UpdateHostState(
		drainingHosts[0],
		host.HostState_HOST_STATE_DOWN,
		host.HostState_HOST_STATE_DRAINING)
	suite.NoError(err)
	suite.Empty(
		maintenanceHostInfoMap.GetDownHostInfos([]string{drainingHosts[0]}))
	suite.NotEmpty(
		maintenanceHostInfoMap.GetDrainingHostInfos([]string{drainingHosts[0]}))

	// Test UpdateHostState errors
	// Test 'invalid current state' error
	err = maintenanceHostInfoMap.UpdateHostState(
		drainingHosts[0],
		host.HostState_HOST_STATE_DRAINED,
		host.HostState_HOST_STATE_DOWN)
	suite.Error(err)
	// Test 'invalid target state' error
	err = maintenanceHostInfoMap.UpdateHostState(
		drainingHosts[0],
		host.HostState_HOST_STATE_DRAINING,
		host.HostState_HOST_STATE_UP)
	suite.Error(err)
	// Test 'host not in expected state' error
	err = maintenanceHostInfoMap.UpdateHostState(
		"invalidHost",
		host.HostState_HOST_STATE_DRAINING,
		host.HostState_HOST_STATE_DOWN)
	suite.Error(err)
	err = maintenanceHostInfoMap.UpdateHostState(
		"invalidHost",
		host.HostState_HOST_STATE_DOWN,
		host.HostState_HOST_STATE_DRAINING)
	suite.Error(err)

	// Test RemoveHostInfos
	maintenanceHostInfoMap.RemoveHostInfos(drainingHosts)
	suite.Empty(maintenanceHostInfoMap.GetDrainingHostInfos([]string{}))
	suite.NotEmpty(maintenanceHostInfoMap.GetDownHostInfos([]string{}))

	maintenanceHostInfoMap.RemoveHostInfos(downHosts)
	suite.Empty(maintenanceHostInfoMap.GetDrainingHostInfos([]string{}))
	suite.Empty(maintenanceHostInfoMap.GetDownHostInfos([]string{}))
}

func TestHostMapTestSuite(t *testing.T) {
	suite.Run(t, new(HostMapTestSuite))
}
