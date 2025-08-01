//go:build !windows

package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/testutil"
	testdaemon "github.com/moby/moby/v2/testutil/daemon"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
)

func setPortConfig(portConfig []swarm.PortConfig) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.EndpointSpec == nil {
			s.Spec.EndpointSpec = &swarm.EndpointSpec{}
		}
		s.Spec.EndpointSpec.Ports = portConfig
	}
}

func (s *DockerSwarmSuite) TestAPIServiceUpdatePort(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a service with a port mapping of 8080:8081.
	portConfig := []swarm.PortConfig{{TargetPort: 8081, PublishedPort: 8080}}
	serviceID := d.CreateService(ctx, c, simpleTestService, setInstances(1), setPortConfig(portConfig))
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// Update the service: changed the port mapping from 8080:8081 to 8082:8083.
	updatedPortConfig := []swarm.PortConfig{{TargetPort: 8083, PublishedPort: 8082}}
	remoteService := d.GetService(ctx, c, serviceID)
	d.UpdateService(ctx, c, remoteService, setPortConfig(updatedPortConfig))

	// Inspect the service and verify port mapping.
	updatedService := d.GetService(ctx, c, serviceID)
	assert.Assert(c, updatedService.Spec.EndpointSpec != nil)
	assert.Equal(c, len(updatedService.Spec.EndpointSpec.Ports), 1)
	assert.Equal(c, updatedService.Spec.EndpointSpec.Ports[0].TargetPort, uint32(8083))
	assert.Equal(c, updatedService.Spec.EndpointSpec.Ports[0].PublishedPort, uint32(8082))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesEmptyList(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	services := d.ListServices(ctx, c)
	assert.Assert(c, services != nil)
	assert.Assert(c, len(services) == 0, "services: %#v", services)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesCreate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	instances := 2
	id := d.CreateService(ctx, c, simpleTestService, setInstances(instances))
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	client := d.NewClientT(c)
	defer client.Close()

	options := swarm.ServiceInspectOptions{InsertDefaults: true}

	// insertDefaults inserts UpdateConfig when service is fetched by ID
	resp, _, err := client.ServiceInspectWithRaw(ctx, id, options)
	out := fmt.Sprintf("%+v", resp)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "UpdateConfig"))

	// insertDefaults inserts UpdateConfig when service is fetched by ID
	resp, _, err = client.ServiceInspectWithRaw(ctx, "top", options)
	out = fmt.Sprintf("%+v", resp)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "UpdateConfig"))

	service := d.GetService(ctx, c, id)
	instances = 5
	d.UpdateService(ctx, c, service, setInstances(instances))
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	d.RemoveService(ctx, c, service.ID)
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesMultipleAgents(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)
	d3 := s.AddDaemon(ctx, c, true, false)

	time.Sleep(1 * time.Second) // make sure all daemons are ready to accept tasks

	instances := 9
	id := d1.CreateService(ctx, c, simpleTestService, setInstances(instances))

	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d3.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	// reconciliation on d2 node down
	d2.Stop(c)

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	// test downscaling
	instances = 5
	d1.UpdateService(ctx, c, d1.GetService(ctx, c, id), setInstances(instances))
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesCreateGlobal(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)
	d3 := s.AddDaemon(ctx, c, true, false)

	d1.CreateService(ctx, c, simpleTestService, setGlobalMode)

	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d3.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	d4 := s.AddDaemon(ctx, c, true, false)
	d5 := s.AddDaemon(ctx, c, true, false)

	poll.WaitOn(c, pollCheck(c, d4.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d5.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(ctx, c, true, i == 0)
	}
	// wait for nodes ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckNodeReadyCount(ctx), checker.Equals(nodeCount)), poll.WithTimeout(5*time.Second))

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "busybox:test"

	// create a different tag
	for _, d := range daemons {
		out, err := d.Cmd("tag", image1, image2)
		assert.NilError(c, err, out)
	}

	// create service
	instances := 5
	parallelism := 2
	rollbackParallelism := 3
	id := daemons[0].CreateService(ctx, c, serviceForUpdate, setInstances(instances))

	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// issue service update
	service := daemons[0].GetService(ctx, c, id)
	daemons[0].UpdateService(ctx, c, service, setImage(image2))

	// first batch
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - parallelism, image2: parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 2nd batch
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 3nd batch
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image2: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// Roll back to the previous version. This uses the CLI because
	// rollback used to be a client-side operation.
	out, err := daemons[0].Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	// first batch
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image2: instances - rollbackParallelism, image1: rollbackParallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 2nd batch
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesUpdateStartFirst(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "testhealth:latest"

	// service started from this image won't pass health check
	result := cli.BuildCmd(c, image2, cli.Daemon(d),
		build.WithDockerfile(`FROM busybox
		HEALTHCHECK --interval=1s --timeout=30s --retries=1024 \
		  CMD cat /status`),
	)
	result.Assert(c, icmd.Success)

	// create service
	instances := 5
	parallelism := 2
	rollbackParallelism := 3
	id := d.CreateService(ctx, c, serviceForUpdate, setInstances(instances), setUpdateOrder(swarm.UpdateOrderStartFirst), setRollbackOrder(swarm.UpdateOrderStartFirst))

	checkStartingTasks := func(expected int) []swarm.Task {
		var startingTasks []swarm.Task
		poll.WaitOn(c, pollCheck(c, func(t *testing.T) (interface{}, string) {
			tasks := d.GetServiceTasks(ctx, t, id)
			startingTasks = nil
			for _, t := range tasks {
				if t.Status.State == swarm.TaskStateStarting {
					startingTasks = append(startingTasks, t)
				}
			}
			return startingTasks, ""
		}, checker.HasLen(expected)), poll.WithTimeout(defaultReconciliationTimeout))

		return startingTasks
	}

	makeTasksHealthy := func(tasks []swarm.Task) {
		for _, t := range tasks {
			containerID := t.Status.ContainerStatus.ContainerID
			d.Cmd("exec", containerID, "touch", "/status")
		}
	}

	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// issue service update
	service := d.GetService(ctx, c, id)
	d.UpdateService(ctx, c, service, setImage(image2))

	// first batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks := checkStartingTasks(parallelism)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// make it healthy
	makeTasksHealthy(startingTasks)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - parallelism, image2: parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 2nd batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks = checkStartingTasks(parallelism)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - parallelism, image2: parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// make it healthy
	makeTasksHealthy(startingTasks)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 3nd batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks = checkStartingTasks(1)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// make it healthy
	makeTasksHealthy(startingTasks)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image2: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// Roll back to the previous version. This uses the CLI because
	// rollback is a client-side operation.
	out, err := d.Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	// first batch
	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image2: instances - rollbackParallelism, image1: rollbackParallelism})), poll.WithTimeout(defaultReconciliationTimeout))

	// 2nd batch
	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesFailedUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(ctx, c, true, i == 0)
	}
	// wait for nodes ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckNodeReadyCount(ctx), checker.Equals(nodeCount)), poll.WithTimeout(5*time.Second))

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "busybox:badtag"

	// create service
	instances := 5
	id := daemons[0].CreateService(ctx, c, serviceForUpdate, setInstances(instances))

	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))

	// issue service update
	service := daemons[0].GetService(ctx, c, id)
	daemons[0].UpdateService(ctx, c, service, setImage(image2), setFailureAction(swarm.UpdateFailureActionPause), setMaxFailureRatio(0.25), setParallelism(1))

	// should update 2 tasks and then pause
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceUpdateState(ctx, id), checker.Equals(swarm.UpdateStatePaused)), poll.WithTimeout(defaultReconciliationTimeout))
	v, _ := daemons[0].CheckServiceRunningTasks(ctx, id)(c)
	assert.Assert(c, v == instances-2)

	// Roll back to the previous version. This uses the CLI because
	// rollback used to be a client-side operation.
	out, err := daemons[0].Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	poll.WaitOn(c, pollCheck(c, daemons[0].CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{image1: instances})), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmServiceConstraintRole(c *testing.T) {
	ctx := testutil.GetContext(c)
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(ctx, c, true, i == 0)
	}
	// wait for nodes ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckNodeReadyCount(ctx), checker.Equals(nodeCount)), poll.WithTimeout(5*time.Second))

	// create service
	constraints := []string{"node.role==worker"}
	instances := 3
	id := daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	// validate tasks are running on worker nodes
	tasks := daemons[0].GetServiceTasks(ctx, c, id)
	for _, task := range tasks {
		node := daemons[0].GetNode(ctx, c, task.NodeID)
		assert.Equal(c, node.Spec.Role, swarm.NodeRoleWorker)
	}
	// remove service
	daemons[0].RemoveService(ctx, c, id)

	// create service
	constraints = []string{"node.role!=worker"}
	id = daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	// validate tasks are running on manager nodes
	for _, task := range tasks {
		node := daemons[0].GetNode(ctx, c, task.NodeID)
		assert.Equal(c, node.Spec.Role, swarm.NodeRoleManager)
	}
	// remove service
	daemons[0].RemoveService(ctx, c, id)

	// create service
	constraints = []string{"node.role==nosuchrole"}
	id = daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	// validate tasks are not assigned to any node
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	for _, task := range tasks {
		assert.Equal(c, task.NodeID, "")
	}
}

func (s *DockerSwarmSuite) TestAPISwarmServiceConstraintLabel(c *testing.T) {
	ctx := testutil.GetContext(c)
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(ctx, c, true, i == 0)
	}
	// wait for nodes ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckNodeReadyCount(ctx), checker.Equals(nodeCount)), poll.WithTimeout(5*time.Second))
	nodes := daemons[0].ListNodes(ctx, c)
	assert.Equal(c, len(nodes), nodeCount)

	// add labels to nodes
	daemons[0].UpdateNode(ctx, c, nodes[0].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"security": "high",
		}
	})
	for i := 1; i < nodeCount; i++ {
		daemons[0].UpdateNode(ctx, c, nodes[i].ID, func(n *swarm.Node) {
			n.Spec.Annotations.Labels = map[string]string{
				"security": "low",
			}
		})
	}

	// create service
	instances := 3
	constraints := []string{"node.labels.security==high"}
	id := daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	tasks := daemons[0].GetServiceTasks(ctx, c, id)
	// validate all tasks are running on nodes[0]
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == nodes[0].ID)
	}
	// remove service
	daemons[0].RemoveService(ctx, c, id)

	// create service
	constraints = []string{"node.labels.security!=high"}
	id = daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	// validate all tasks are NOT running on nodes[0]
	for _, task := range tasks {
		assert.Assert(c, task.NodeID != nodes[0].ID)
	}
	// remove service
	daemons[0].RemoveService(ctx, c, id)

	constraints = []string{"node.labels.security==medium"}
	id = daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	// validate tasks are not assigned
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == "")
	}
	// remove service
	daemons[0].RemoveService(ctx, c, id)

	// multiple constraints
	constraints = []string{
		"node.labels.security==high",
		fmt.Sprintf("node.id==%s", nodes[1].ID),
	}
	id = daemons[0].CreateService(ctx, c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	// validate tasks are not assigned
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == "")
	}
	// make nodes[1] fulfills the constraints
	daemons[0].UpdateNode(ctx, c, nodes[1].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"security": "high",
		}
	})
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	tasks = daemons[0].GetServiceTasks(ctx, c, id)
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == nodes[1].ID)
	}
}

func (s *DockerSwarmSuite) TestAPISwarmServicePlacementPrefs(c *testing.T) {
	ctx := testutil.GetContext(c)

	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(ctx, c, true, i == 0)
	}
	// wait for nodes ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckNodeReadyCount(ctx), checker.Equals(nodeCount)), poll.WithTimeout(5*time.Second))
	nodes := daemons[0].ListNodes(ctx, c)
	assert.Equal(c, len(nodes), nodeCount)

	// add labels to nodes
	daemons[0].UpdateNode(ctx, c, nodes[0].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"rack": "a",
		}
	})
	for i := 1; i < nodeCount; i++ {
		daemons[0].UpdateNode(ctx, c, nodes[i].ID, func(n *swarm.Node) {
			n.Spec.Annotations.Labels = map[string]string{
				"rack": "b",
			}
		})
	}

	// create service
	instances := 4
	prefs := []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.rack"}}}
	id := daemons[0].CreateService(ctx, c, simpleTestService, setPlacementPrefs(prefs), setInstances(instances))
	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, daemons[0].CheckServiceRunningTasks(ctx, id), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	tasks := daemons[0].GetServiceTasks(ctx, c, id)
	// validate all tasks are running on nodes[0]
	tasksOnNode := make(map[string]int)
	for _, task := range tasks {
		tasksOnNode[task.NodeID]++
	}
	assert.Assert(c, tasksOnNode[nodes[0].ID] == 2)
	assert.Assert(c, tasksOnNode[nodes[1].ID] == 1)
	assert.Assert(c, tasksOnNode[nodes[2].ID] == 1)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesStateReporting(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	testRequires(c, DaemonIsLinux)
	ctx := testutil.GetContext(c)

	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, false)

	time.Sleep(1 * time.Second) // make sure all daemons are ready to accept

	instances := 9
	d1.CreateService(ctx, c, simpleTestService, setInstances(instances))

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	getContainers := func() map[string]*daemon.Daemon {
		m := make(map[string]*daemon.Daemon)
		for _, d := range []*daemon.Daemon{d1, d2, d3} {
			for _, id := range d.ActiveContainers(testutil.GetContext(c), c) {
				m[id] = d
			}
		}
		return m
	}

	containers := getContainers()
	assert.Assert(c, len(containers) == instances)
	var toRemove string
	for i := range containers {
		toRemove = i
	}

	_, err := containers[toRemove].Cmd("stop", toRemove)
	assert.NilError(c, err)

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	containers2 := getContainers()
	assert.Assert(c, len(containers2) == instances)
	for i := range containers {
		if i == toRemove {
			assert.Assert(c, is.Nil(containers2[i]))
		} else {
			assert.Assert(c, containers2[i] != nil)
		}
	}

	containers = containers2
	for i := range containers {
		toRemove = i
	}

	// try with killing process outside of docker
	pidStr, err := containers[toRemove].Cmd("inspect", "-f", "{{.State.Pid}}", toRemove)
	assert.NilError(c, err)
	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	assert.NilError(c, err)
	assert.NilError(c, unix.Kill(pid, unix.SIGKILL))

	time.Sleep(time.Second) // give some time to handle the signal

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	containers2 = getContainers()
	assert.Assert(c, len(containers2) == instances)
	for i := range containers {
		if i == toRemove {
			assert.Assert(c, is.Nil(containers2[i]))
		} else {
			assert.Assert(c, containers2[i] != nil)
		}
	}
}
