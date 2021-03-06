// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package swarm

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/fsouza/go-dockerclient"
	"github.com/pkg/errors"
	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/log"
	tsuruNet "github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/cluster"
	"github.com/tsuru/tsuru/provision/dockercommon"
	"github.com/tsuru/tsuru/provision/nodecontainer"
	"github.com/tsuru/tsuru/provision/servicecommon"
)

const (
	provisionerName = "swarm"
)

var swarmConfig swarmProvisionerConfig

type swarmProvisioner struct{}

var (
	_ provision.Provisioner              = &swarmProvisioner{}
	_ provision.ShellProvisioner         = &swarmProvisioner{}
	_ provision.ExecutableProvisioner    = &swarmProvisioner{}
	_ provision.MessageProvisioner       = &swarmProvisioner{}
	_ provision.InitializableProvisioner = &swarmProvisioner{}
	_ provision.NodeProvisioner          = &swarmProvisioner{}
	_ provision.NodeContainerProvisioner = &swarmProvisioner{}
	_ provision.SleepableProvisioner     = &swarmProvisioner{}
	_ provision.BuilderDeploy            = &swarmProvisioner{}
	_ provision.VolumeProvisioner        = &swarmProvisioner{}
	_ cluster.InitClusterProvisioner     = &swarmProvisioner{}
	// _ provision.RollbackableDeployer     = &swarmProvisioner{}
	// _ provision.RebuildableDeployer      = &swarmProvisioner{}
	// _ provision.OptionalLogsProvisioner  = &swarmProvisioner{}
	// _ provision.UnitStatusProvisioner    = &swarmProvisioner{}
	// _ provision.NodeRebalanceProvisioner = &swarmProvisioner{}
	// _ provision.AppFilterProvisioner     = &swarmProvisioner{}
	// _ provision.ExtensibleProvisioner    = &swarmProvisioner{}
	// _ builder.PlatformBuilder            = &swarmProvisioner{}
)

type swarmProvisionerConfig struct {
	swarmPort int
}

func init() {
	provision.Register(provisionerName, func() (provision.Provisioner, error) {
		return &swarmProvisioner{}, nil
	})
}

func (p *swarmProvisioner) Initialize() error {
	var err error
	swarmConfig.swarmPort, err = config.GetInt("swarm:swarm-port")
	if err != nil {
		swarmConfig.swarmPort = 2377
	}
	return nil
}

func (p *swarmProvisioner) GetName() string {
	return provisionerName
}

func (p *swarmProvisioner) Provision(a provision.App) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	_, err = client.CreateNetwork(docker.CreateNetworkOptions{
		Name:           networkNameForApp(a),
		Driver:         "overlay",
		CheckDuplicate: true,
		IPAM: docker.IPAMOptions{
			Driver: "default",
		},
	})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *swarmProvisioner) Destroy(a provision.App) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	multiErrors := tsuruErrors.NewMultiError()
	processes, err := image.AllAppProcesses(a.GetName())
	if err != nil {
		multiErrors.Add(err)
	}
	for _, p := range processes {
		name := serviceNameForApp(a, p)
		err = client.RemoveService(docker.RemoveServiceOptions{
			ID: name,
		})
		if err != nil {
			if _, notFound := err.(*docker.NoSuchService); !notFound {
				multiErrors.Add(errors.WithStack(err))
			}
		}
	}
	err = client.RemoveNetwork(networkNameForApp(a))
	if err != nil {
		multiErrors.Add(errors.WithStack(err))
	}
	if multiErrors.Len() > 0 {
		return multiErrors
	}
	return nil
}

func (p *swarmProvisioner) AddUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, int(units), processName)
}

func (p *swarmProvisioner) RemoveUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, -int(units), processName)
}

func (p *swarmProvisioner) Restart(a provision.App, process string, w io.Writer) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true, Restart: true})
}

func (p *swarmProvisioner) Start(a provision.App, process string) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true})
}

func (p *swarmProvisioner) Stop(a provision.App, process string) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Stop: true})
}

var stateMap = map[swarm.TaskState]provision.Status{
	swarm.TaskStateNew:       provision.StatusCreated,
	swarm.TaskStateAllocated: provision.StatusStarting,
	swarm.TaskStatePending:   provision.StatusStarting,
	swarm.TaskStateAssigned:  provision.StatusStarting,
	swarm.TaskStateAccepted:  provision.StatusStarting,
	swarm.TaskStatePreparing: provision.StatusStarting,
	swarm.TaskStateReady:     provision.StatusStarting,
	swarm.TaskStateStarting:  provision.StatusStarting,
	swarm.TaskStateRunning:   provision.StatusStarted,
	swarm.TaskStateComplete:  provision.StatusStopped,
	swarm.TaskStateShutdown:  provision.StatusStopped,
	swarm.TaskStateFailed:    provision.StatusError,
	swarm.TaskStateRejected:  provision.StatusError,
}

func taskToUnit(task *swarm.Task, service *swarm.Service, node *swarm.Node, a provision.App) provision.Unit {
	host := tsuruNet.URLToHost(node.Status.Addr)
	labels := provision.LabelSet{Labels: service.Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
	return provision.Unit{
		ID:          task.ID,
		AppName:     a.GetName(),
		ProcessName: labels.AppProcess(),
		Type:        a.GetPlatform(),
		IP:          host,
		Status:      stateMap[task.Status.State],
		Address:     &url.URL{},
	}
}

func tasksToUnits(client *clusterClient, tasks []swarm.Task) ([]provision.Unit, error) {
	nodeMap := map[string]*swarm.Node{}
	serviceMap := map[string]*swarm.Service{}
	appsMap := map[string]provision.App{}
	units := []provision.Unit{}
	for _, t := range tasks {
		labels := provision.LabelSet{Labels: t.Spec.ContainerSpec.Labels, Prefix: tsuruLabelPrefix}
		if !labels.IsService() {
			continue
		}
		if t.DesiredState == swarm.TaskStateShutdown ||
			t.NodeID == "" ||
			t.ServiceID == "" {
			continue
		}
		if _, ok := nodeMap[t.NodeID]; !ok {
			node, err := client.InspectNode(t.NodeID)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			nodeMap[t.NodeID] = node
		}
		if _, ok := serviceMap[t.ServiceID]; !ok {
			service, err := client.InspectService(t.ServiceID)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			serviceMap[t.ServiceID] = service
		}
		service := serviceMap[t.ServiceID]
		serviceLabels := provision.LabelSet{Labels: service.Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
		appName := serviceLabels.AppName()
		if _, ok := appsMap[appName]; !ok {
			a, err := app.GetByName(appName)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			appsMap[appName] = a
		}
		units = append(units, taskToUnit(&t, serviceMap[t.ServiceID], nodeMap[t.NodeID], appsMap[appName]))
	}
	return units, nil
}

func (p *swarmProvisioner) Units(a provision.App) ([]provision.Unit, error) {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		if errors.Cause(err) == cluster.ErrNoCluster {
			return []provision.Unit{}, nil
		}
		return nil, err
	}
	l, err := provision.ProcessLabels(provision.ProcessLabelsOpts{App: a, Prefix: tsuruLabelPrefix})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	tasks, err := client.ListTasks(docker.ListTasksOptions{
		Filters: map[string][]string{
			"label": toLabelSelectors(l.ToAppSelector()),
		},
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return tasksToUnits(client, tasks)
}

func (p *swarmProvisioner) RoutableAddresses(a provision.App) ([]url.URL, error) {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return nil, err
	}
	imgID, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		if err != image.ErrNoImagesAvailable {
			return nil, err
		}
		return nil, nil
	}
	webProcessName, err := image.GetImageWebProcessName(imgID)
	if err != nil {
		return nil, err
	}
	if webProcessName == "" {
		return nil, nil
	}
	srvName := serviceNameForApp(a, webProcessName)
	var retries int
	var pubPort uint32
	for retries < 5 && pubPort == 0 {
		if retries > 0 {
			log.Debugf("[swarm-routable-addresses] sleeping for 3 seconds")
			time.Sleep(time.Second * 3)
		}
		srv, errInspect := client.InspectService(srvName)
		if errInspect != nil {
			return nil, errInspect
		}
		log.Debugf("[swarm-routable-addresses] service for app %q: %#+v", a.GetName(), srv)
		if len(srv.Endpoint.Ports) > 0 {
			pubPort = srv.Endpoint.Ports[0].PublishedPort
		}
		retries++
	}
	if pubPort == 0 {
		log.Debugf("[swarm-routable-addresses] no exposed ports for app %q", a.GetName())
		return nil, nil
	}
	nodes, err := client.ListNodes(docker.ListNodesOptions{})
	if err != nil {
		return nil, err
	}
	log.Debugf("[swarm-routable-addresses] valid nodes for app %q: %#+v", a.GetName(), nodes)
	for i := len(nodes) - 1; i >= 0; i-- {
		l := provision.LabelSet{Labels: nodes[i].Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
		if l.NodePool() != a.GetPool() {
			nodes[i], nodes[len(nodes)-1] = nodes[len(nodes)-1], nodes[i]
			nodes = nodes[:len(nodes)-1]
		}
	}
	addrs := make([]url.URL, len(nodes))
	for i, n := range nodes {
		host := tsuruNet.URLToHost(n.Status.Addr)
		addrs[i] = url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", host, pubPort),
		}
	}
	return addrs, nil
}

func findTaskByContainerId(tasks []swarm.Task, unitId string) (*swarm.Task, error) {
	for i, t := range tasks {
		if strings.HasPrefix(t.Status.ContainerStatus.ContainerID, unitId) {
			return &tasks[i], nil
		}
	}
	return nil, &provision.UnitNotFoundError{ID: unitId}
}

func (p *swarmProvisioner) RegisterUnit(a provision.App, unitId string, customData map[string]interface{}) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	l, err := provision.ProcessLabels(provision.ProcessLabelsOpts{App: a, Prefix: tsuruLabelPrefix})
	if err != nil {
		return errors.WithStack(err)
	}
	tasks, err := client.ListTasks(docker.ListTasksOptions{
		Filters: map[string][]string{
			"label": toLabelSelectors(l.ToAppSelector()),
		},
	})
	if err != nil {
		return err
	}
	task, err := findTaskByContainerId(tasks, unitId)
	if err != nil {
		return err
	}
	if customData == nil {
		return nil
	}
	labels := provision.LabelSet{Labels: task.Spec.ContainerSpec.Labels, Prefix: tsuruLabelPrefix}
	if !labels.IsDeploy() {
		return nil
	}
	buildingImage := labels.BuildImage()
	if buildingImage == "" {
		return errors.Errorf("invalid build image label for build task: %#v", task)
	}
	return image.SaveImageCustomData(buildingImage, customData)
}

func (p *swarmProvisioner) ListNodes(addressFilter []string) ([]provision.Node, error) {
	clusters, err := allClusters()
	if err != nil {
		if errors.Cause(err) == cluster.ErrNoCluster {
			return nil, nil
		}
		return nil, err
	}
	var nodeList []provision.Node
	for _, client := range clusters {
		nodes, err := client.ListNodes(docker.ListNodesOptions{})
		if err != nil {
			return nil, err
		}
		var filterMap map[string]struct{}
		if len(addressFilter) > 0 {
			filterMap = map[string]struct{}{}
			for _, addr := range addressFilter {
				filterMap[addr] = struct{}{}
			}
		}
		for i := range nodes {
			wrapped := &swarmNodeWrapper{Node: &nodes[i], provisioner: p, client: client}
			toAdd := true
			if filterMap != nil {
				_, toAdd = filterMap[wrapped.Address()]
			}
			if toAdd {
				nodeList = append(nodeList, wrapped)
			}
		}
	}
	return nodeList, nil
}

func (p *swarmProvisioner) GetNode(address string) (provision.Node, error) {
	nodes, err := p.ListNodes([]string{address})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, provision.ErrNodeNotFound
	}
	return nodes[0], nil
}

func (p *swarmProvisioner) NodeForNodeData(nodeData provision.NodeStatusData) (provision.Node, error) {
	clusters, err := allClusters()
	if err != nil {
		if errors.Cause(err) != cluster.ErrNoCluster {
			return nil, err
		}
	}
	for _, client := range clusters {
		tasks, err := client.ListTasks(docker.ListTasksOptions{})
		if err != nil {
			return nil, err
		}
		var task *swarm.Task
		for _, unitData := range nodeData.Units {
			task, err = findTaskByContainerId(tasks, unitData.ID)
			if err == nil {
				break
			}
			if _, isNotFound := errors.Cause(err).(*provision.UnitNotFoundError); !isNotFound {
				return nil, err
			}
		}
		if task != nil {
			node, err := client.InspectNode(task.NodeID)
			if err != nil {
				if _, notFound := err.(*docker.NoSuchNode); notFound {
					return nil, provision.ErrNodeNotFound
				}
				return nil, err
			}
			return &swarmNodeWrapper{Node: node, provisioner: p, client: client}, nil
		}
	}
	return provision.FindNodeByAddrs(p, nodeData.Addrs)
}

func (p *swarmProvisioner) AddNode(opts provision.AddNodeOptions) error {
	existingClient, err := clusterForPool(opts.Pool)
	if err != nil {
		return err
	}
	tls, err := tlsConfigForCluster(existingClient.Cluster)
	if err != nil {
		return err
	}
	newClient, err := newClient(opts.Address, tls)
	if err != nil {
		return err
	}
	err = dockercommon.WaitDocker(newClient)
	if err != nil {
		return err
	}
	err = joinSwarm(existingClient, newClient, opts.Address)
	if err != nil {
		return err
	}
	dockerInfo, err := newClient.Info()
	if err != nil {
		return errors.WithStack(err)
	}
	nodeData, err := existingClient.InspectNode(dockerInfo.Swarm.NodeID)
	if err != nil {
		return errors.WithStack(err)
	}
	nodeData.Spec.Annotations.Labels = provision.NodeLabels(provision.NodeLabelsOpts{
		IaaSID:       opts.IaaSID,
		Addr:         opts.Address,
		Pool:         opts.Pool,
		CustomLabels: opts.Metadata,
		Prefix:       tsuruLabelPrefix,
	}).ToLabels()
	err = existingClient.UpdateNode(dockerInfo.Swarm.NodeID, docker.UpdateNodeOptions{
		Version:  nodeData.Version.Index,
		NodeSpec: nodeData.Spec,
	})
	return errors.WithStack(err)
}

func (p *swarmProvisioner) RemoveNode(opts provision.RemoveNodeOptions) error {
	node, err := p.GetNode(opts.Address)
	if err != nil {
		return err
	}
	client, err := clusterForPool(node.Pool())
	if err != nil {
		return err
	}
	swarmNode := node.(*swarmNodeWrapper).Node
	if opts.Rebalance {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityDrain
		err = client.UpdateNode(swarmNode.ID, docker.UpdateNodeOptions{
			NodeSpec: swarmNode.Spec,
			Version:  swarmNode.Version.Index,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	nodes, err := client.ListNodes(docker.ListNodesOptions{})
	if err != nil {
		return errors.WithStack(err)
	}
	if len(nodes) == 1 {
		return errors.New("cannot remove last node from swarm, remove the cluster from tsuru to remove it")
	}
	err = client.RemoveNode(docker.RemoveNodeOptions{
		ID:    swarmNode.ID,
		Force: true,
	})
	return errors.WithStack(err)
}

func (p *swarmProvisioner) UpdateNode(opts provision.UpdateNodeOptions) error {
	node, err := p.GetNode(opts.Address)
	if err != nil {
		return err
	}
	swarmNode := node.(*swarmNodeWrapper).Node
	if opts.Disable {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityPause
	} else if opts.Enable {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityActive
	}
	if swarmNode.Spec.Annotations.Labels == nil {
		swarmNode.Spec.Annotations.Labels = map[string]string{}
	}
	if opts.Pool != "" {
		baseNodeLabels := provision.NodeLabels(provision.NodeLabelsOpts{
			Pool:   opts.Pool,
			Prefix: tsuruLabelPrefix,
		})
		for k, v := range baseNodeLabels.ToLabels() {
			swarmNode.Spec.Annotations.Labels[k] = v
		}
	}
	for k, v := range opts.Metadata {
		k = tsuruLabelPrefix + strings.TrimPrefix(k, tsuruLabelPrefix)
		if v == "" {
			delete(swarmNode.Spec.Annotations.Labels, k)
		} else {
			swarmNode.Spec.Annotations.Labels[k] = v
		}
	}
	client, err := clusterForPool(node.Pool())
	if err != nil {
		return err
	}
	err = client.UpdateNode(swarmNode.ID, docker.UpdateNodeOptions{
		NodeSpec: swarmNode.Spec,
		Version:  swarmNode.Version.Index,
	})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *swarmProvisioner) GetDockerClient(a provision.App) (provision.BuilderDockerClient, error) {
	if a == nil {
		clusters, err := allClusters()
		if err != nil {
			if errors.Cause(err) == cluster.ErrNoCluster {
				return nil, nil
			}
			return nil, err
		}
		return clusters[0].Client, nil
	}
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return nil, err
	}
	return &dockercommon.ClientWithTimeout{Client: client.Client}, nil
}

func (p *swarmProvisioner) CleanImage(appName, imgName string) {
	p.cleanImageInNodes(imgName)
}

func (p *swarmProvisioner) Deploy(a provision.App, buildImageID string, evt *event.Event) (string, error) {
	if !strings.HasSuffix(buildImageID, "-builder") {
		err := deployProcesses(a, buildImageID, nil)
		if err != nil {
			return "", err
		}
		return buildImageID, nil
	}
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return "", err
	}
	deployImage, err := image.AppNewImageName(a.GetName())
	if err != nil {
		return "", err
	}
	cmds := dockercommon.DeployCmds(a)
	srvID, task, err := runOnceBuildCmds(client, a, cmds, buildImageID, deployImage, evt)
	if srvID != "" {
		defer removeServiceAndLog(client, srvID)
	}
	if err != nil {
		return "", errors.WithStack(err)
	}
	nodeClient, err := clientForNode(client, task.NodeID)
	if err != nil {
		return "", err
	}
	_, err = commitPushBuildImage(nodeClient, deployImage, task.Status.ContainerStatus.ContainerID, a)
	if err != nil {
		return "", errors.WithStack(err)
	}
	err = deployProcesses(a, deployImage, nil)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return deployImage, nil
}

func (p *swarmProvisioner) Shell(opts provision.ShellOptions) error {
	client, err := clusterForPool(opts.App.GetPool())
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, opts.App, opts.Unit)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		if opts.Unit != "" {
			return &provision.UnitNotFoundError{ID: opts.Unit}
		}
		return provision.ErrEmptyApp
	}
	nodeClient, err := clientForNode(client, tasks[0].NodeID)
	if err != nil {
		return err
	}
	cmds := []string{"/usr/bin/env", "TERM=" + opts.Term, "bash", "-l"}
	execCreateOpts := docker.CreateExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmds,
		Container:    tasks[0].Status.ContainerStatus.ContainerID,
		Tty:          true,
	}
	exec, err := nodeClient.CreateExec(execCreateOpts)
	if err != nil {
		return errors.WithStack(err)
	}
	startExecOptions := docker.StartExecOptions{
		InputStream:  opts.Conn,
		OutputStream: opts.Conn,
		ErrorStream:  opts.Conn,
		Tty:          true,
		RawTerminal:  true,
	}
	errs := make(chan error, 1)
	go func() {
		errs <- nodeClient.StartExec(exec.ID, startExecOptions)
	}()
	execInfo, err := nodeClient.InspectExec(exec.ID)
	for !execInfo.Running && err == nil {
		select {
		case startErr := <-errs:
			return startErr
		default:
			execInfo, err = nodeClient.InspectExec(exec.ID)
		}
	}
	if err != nil {
		return errors.WithStack(err)
	}
	nodeClient.ResizeExecTTY(exec.ID, opts.Height, opts.Width)
	return <-errs
}

func (p *swarmProvisioner) ExecuteCommand(stdout, stderr io.Writer, a provision.App, cmd string, args ...string) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, a, "")
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return provision.ErrEmptyApp
	}
	for _, t := range tasks {
		err := execInTaskContainer(client, &t, stdout, stderr, cmd, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *swarmProvisioner) ExecuteCommandOnce(stdout, stderr io.Writer, a provision.App, cmd string, args ...string) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, a, "")
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return provision.ErrEmptyApp
	}
	return execInTaskContainer(client, &tasks[0], stdout, stderr, cmd, args...)
}

func (p *swarmProvisioner) ExecuteCommandIsolated(stdout, stderr io.Writer, a provision.App, cmd string, args ...string) error {
	if a.GetDeploys() == 0 {
		return errors.New("commands can only be executed after the first deploy")
	}
	img, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	opts := tsuruServiceOpts{
		app:           a,
		image:         img,
		isIsolatedRun: true,
	}
	cmds := []string{"/bin/bash", "-lc", cmd}
	cmds = append(cmds, args...)
	serviceID, _, err := runOnceCmds(client, opts, cmds, stdout, stderr)
	if serviceID != "" {
		removeServiceAndLog(client, serviceID)
	}
	return err
}

type nodeContainerManager struct{}

func (m *nodeContainerManager) DeployNodeContainer(config *nodecontainer.NodeContainerConfig, pool string, filter servicecommon.PoolFilter, placementOnly bool) error {
	serviceSpec, err := serviceSpecForNodeContainer(config, pool, filter)
	if err != nil {
		return err
	}
	err = forEachCluster(func(client *clusterClient) error {
		_, upsertErr := upsertService(serviceSpec, client, placementOnly)
		return upsertErr
	})
	if err == cluster.ErrNoCluster {
		return nil
	}
	return err
}

func (p *swarmProvisioner) UpgradeNodeContainer(name string, pool string, writer io.Writer) error {
	m := nodeContainerManager{}
	return servicecommon.UpgradeNodeContainer(&m, name, pool, writer)
}

func (p *swarmProvisioner) RemoveNodeContainer(name string, pool string, writer io.Writer) error {
	err := forEachCluster(func(client *clusterClient) error {
		return client.RemoveService(docker.RemoveServiceOptions{ID: nodeContainerServiceName(name, pool)})
	})
	if err == cluster.ErrNoCluster {
		return nil
	}
	return err
}

func (p *swarmProvisioner) StartupMessage() (string, error) {
	out := "Swarm provisioner reports the following nodes:\n"
	clusters, err := allClusters()
	if err != nil {
		if errors.Cause(err) == cluster.ErrNoCluster {
			return out + "    No Swarm node available.\n", nil
		}
		return "", err
	}
	for _, client := range clusters {
		nodeList, err := client.ListNodes(docker.ListNodesOptions{})
		if err != nil {
			return "", err
		}
		for _, node := range nodeList {
			addr := nodeAddr(client, &node)
			out += fmt.Sprintf("    Swarm node: %s [%s] [%s]\n", addr, node.Status.State, node.Spec.Role)
		}
	}
	return out, nil
}

func (p *swarmProvisioner) DeleteVolume(volumeName, pool string) error {
	clusters, err := allClusters()
	if err != nil {
		if errors.Cause(err) == cluster.ErrNoCluster {
			return nil
		}
		return err
	}
	for _, client := range clusters {
		nodes, err := client.ListNodes(docker.ListNodesOptions{})
		if err != nil {
			return err
		}
		for _, n := range nodes {
			nodeClient, err := clientForNode(client, n.ID)
			if err != nil {
				return err
			}
			if err := nodeClient.RemoveVolume(volumeName); err != docker.ErrNoSuchVolume && err != nil {
				return err
			}
		}
	}
	return nil
}

func deployProcesses(a provision.App, newImg string, updateSpec servicecommon.ProcessSpec) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	manager := &serviceManager{
		client: client,
	}
	return servicecommon.RunServicePipeline(manager, a, newImg, updateSpec)
}

type serviceManager struct {
	client *clusterClient
}

func (m *serviceManager) RemoveService(a provision.App, process string) error {
	srvName := serviceNameForApp(a, process)
	err := m.client.RemoveService(docker.RemoveServiceOptions{ID: srvName})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (m *serviceManager) CurrentLabels(a provision.App, process string) (*provision.LabelSet, error) {
	srvName := serviceNameForApp(a, process)
	srv, err := m.client.InspectService(srvName)
	if err != nil {
		if _, isNotFound := err.(*docker.NoSuchService); isNotFound {
			return nil, nil
		}
		return nil, errors.WithStack(err)
	}
	return &provision.LabelSet{Labels: srv.Spec.Labels, Prefix: tsuruLabelPrefix}, nil
}

func (m *serviceManager) DeployService(a provision.App, process string, labels *provision.LabelSet, replicas int, imgID string) error {
	srvName := serviceNameForApp(a, process)
	srv, err := m.client.InspectService(srvName)
	if err != nil {
		if _, isNotFound := err.(*docker.NoSuchService); !isNotFound {
			return errors.WithStack(err)
		}
	}
	spec, err := serviceSpecForApp(tsuruServiceOpts{
		app:      a,
		process:  process,
		image:    imgID,
		labels:   labels,
		replicas: replicas,
	})
	if err != nil {
		return err
	}
	if srv == nil {
		_, err = m.client.CreateService(docker.CreateServiceOptions{
			ServiceSpec: *spec,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	} else {
		srv.Spec = *spec
		err = m.client.UpdateService(srv.ID, docker.UpdateServiceOptions{
			Version:     srv.Version.Index,
			ServiceSpec: srv.Spec,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func runOnceBuildCmds(client *clusterClient, a provision.App, cmds []string, imgID, buildingImage string, w io.Writer) (string, *swarm.Task, error) {
	opts := tsuruServiceOpts{
		app:        a,
		image:      imgID,
		isDeploy:   true,
		buildImage: buildingImage,
	}
	return runOnceCmds(client, opts, cmds, w, w)
}

func runOnceCmds(client *clusterClient, opts tsuruServiceOpts, cmds []string, stdout, stderr io.Writer) (string, *swarm.Task, error) {
	spec, err := serviceSpecForApp(opts)
	if err != nil {
		return "", nil, err
	}
	spec.TaskTemplate.ContainerSpec.Command = cmds
	spec.TaskTemplate.RestartPolicy.Condition = swarm.RestartPolicyConditionNone
	srv, err := client.CreateService(docker.CreateServiceOptions{
		ServiceSpec: *spec,
	})
	if err != nil {
		return "", nil, errors.WithStack(err)
	}
	createdID := srv.ID
	tasks, err := waitForTasks(client, createdID, swarm.TaskStateShutdown, swarm.TaskStateComplete)
	if err != nil {
		return createdID, nil, err
	}
	nodeClient, err := clientForNode(client, tasks[0].NodeID)
	if err != nil {
		return createdID, nil, err
	}
	contID := tasks[0].Status.ContainerStatus.ContainerID
	attachOpts := docker.AttachToContainerOptions{
		Container:    contID,
		OutputStream: stdout,
		ErrorStream:  stderr,
		Logs:         true,
		Stdout:       true,
		Stderr:       true,
		Stream:       true,
	}
	exitCode, err := safeAttachWaitContainer(nodeClient, attachOpts)
	if err != nil {
		return createdID, nil, err
	}
	if exitCode != 0 {
		return createdID, nil, errors.Errorf("unexpected result code for build container: %d", exitCode)
	}
	return createdID, &tasks[0], nil
}

func (p *swarmProvisioner) Sleep(a provision.App, process string) error {
	client, err := clusterForPool(a.GetPool())
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Stop: true, Sleep: true})
}

func (p *swarmProvisioner) InitializeCluster(c *cluster.Cluster) error {
	client, err := newClusterClient(c)
	if err != nil {
		return err
	}
	host := tsuruNet.URLToHost(client.Cluster.Addresses[0])
	_, err = client.InitSwarm(docker.InitSwarmOptions{
		InitRequest: swarm.InitRequest{
			ListenAddr:    fmt.Sprintf("0.0.0.0:%d", swarmConfig.swarmPort),
			AdvertiseAddr: host,
		},
	})
	if err != nil && errors.Cause(err) != docker.ErrNodeAlreadyInSwarm {
		return errors.WithStack(err)
	}
	return nil
}
