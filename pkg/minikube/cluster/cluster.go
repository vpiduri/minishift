/*
Copyright (C) 2016 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/machine/drivers/virtualbox"
	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/jimmidyson/minishift/pkg/minikube/constants"
	"github.com/jimmidyson/minishift/pkg/minikube/sshutil"
	"github.com/jimmidyson/minishift/pkg/util"
	kubeApi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
)

var (
	certs = []string{"apiserver.crt", "apiserver.key"}
)

//This init function is used to set the logtostderr variable to false so that INFO level log info does not clutter the CLI
//INFO lvl logging is displayed due to the kubernetes api calling flag.Set("logtostderr", "true") in its init()
//see: https://github.com/kubernetes/kubernetes/blob/master/pkg/util/logs.go#L32-34
func init() {
	flag.Set("logtostderr", "false")
}

// StartHost starts a host VM.
func StartHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	if exists, err := api.Exists(constants.MachineName); err != nil {
		return nil, fmt.Errorf("Error checking if host exists: %s", err)
	} else if exists {
		glog.Infoln("Machine exists!")
		h, err := api.Load(constants.MachineName)
		if err != nil {
			return nil, fmt.Errorf("Error loading existing host: %s", err)
		}
		s, err := h.Driver.GetState()
		if err != nil {
			return nil, fmt.Errorf("Error getting state for host: %s", err)
		}
		if s != state.Running {
			if err := h.Driver.Start(); err != nil {
				return nil, fmt.Errorf("Error starting stopped host: %s", err)
			}
			if err := api.Save(h); err != nil {
				return nil, fmt.Errorf("Error saving started host: %s", err)
			}
		}
		if err := h.ConfigureAuth(); err != nil {
			return nil, fmt.Errorf("Error configuring auth on host: %s", err)
		}
		return h, nil
	} else {
		return createHost(api, config)
	}
}

// StopHost stops the host VM.
func StopHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return err
	}
	if err := host.Stop(); err != nil {
		return err
	}
	return nil
}

type multiError struct {
	Errors []error
}

func (m *multiError) Collect(err error) {
	if err != nil {
		m.Errors = append(m.Errors, err)
	}
}

func (m multiError) ToError() error {
	if len(m.Errors) == 0 {
		return nil
	}

	errStrings := []string{}
	for _, err := range m.Errors {
		errStrings = append(errStrings, err.Error())
	}
	return fmt.Errorf(strings.Join(errStrings, "\n"))
}

// DeleteHost deletes the host VM.
func DeleteHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return err
	}
	m := multiError{}
	m.Collect(host.Driver.Remove())
	m.Collect(api.Remove(constants.MachineName))
	return m.ToError()
}

// GetHostStatus gets the status of the host VM.
func GetHostStatus(api libmachine.API) (string, error) {
	dne := "Does Not Exist"
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return "", err
	}
	if !exists {
		return dne, nil
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return "", err
	}

	s, err := host.Driver.GetState()
	if s.String() == "" {
		return dne, err
	}
	return s.String(), err
}

type sshAble interface {
	RunSSHCommand(string) (string, error)
}

// MachineConfig contains the parameters used to start a cluster.
type MachineConfig struct {
	MinikubeISO string
	Memory      int
	CPUs        int
	DiskSize    int
	VMDriver    string
}

// StartCluster starts a k8s cluster on the specified Host.
func StartCluster(h sshAble) error {
	commands := []string{stopCommand, GetStartCommand()}

	for _, cmd := range commands {
		glog.Infoln(cmd)
		output, err := h.RunSSHCommand(cmd)
		glog.Infoln(output)
		if err != nil {
			return err
		}
	}

	return nil
}

type fileToCopy struct {
	AssetName   string
	TargetDir   string
	TargetName  string
	Permissions string
}

var assets = []fileToCopy{
	{
		AssetName:   "out/openshift",
		TargetDir:   "/usr/local/bin",
		TargetName:  "openshift",
		Permissions: "0777",
	},
}

func UpdateCluster(d drivers.Driver) error {
	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return err
	}

	for _, a := range assets {
		contents, err := Asset(a.AssetName)
		if err != nil {
			glog.Infof("Error loading asset %s: %s", a.AssetName, err)
			return err
		}

		if err := sshutil.Transfer(contents, a.TargetDir, a.TargetName, a.Permissions, client); err != nil {
			return err
		}
	}
	return nil
}

// SetupCerts gets the generated credentials required to talk to the APIServer.
func SetupCerts(d drivers.Driver) error {
	localPath := constants.Minipath
	ipStr, err := d.GetIP()
	if err != nil {
		return err
	}
	glog.Infoln("Setting up certificates for IP: %s", ipStr)

	ip := net.ParseIP(ipStr)
	publicPath := filepath.Join(localPath, "apiserver.crt")
	privatePath := filepath.Join(localPath, "apiserver.key")
	if err := GenerateCerts(publicPath, privatePath, ip); err != nil {
		return err
	}

	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return err
	}

	for _, cert := range certs {
		p := filepath.Join(localPath, cert)
		data, err := ioutil.ReadFile(p)
		if err != nil {
			return err
		}
		if err := sshutil.Transfer(data, util.DefaultCertPath, cert, "0644", client); err != nil {
			return err
		}
	}
	return nil
}

func createHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	var driver interface{}

	switch config.VMDriver {
	case "virtualbox":
		d := virtualbox.NewDriver(constants.MachineName, constants.Minipath)
		d.Boot2DockerURL = config.MinikubeISO
		d.Memory = config.Memory
		d.CPU = config.CPUs
		d.DiskSize = int(config.DiskSize)
		driver = d
	case "vmwarefusion":
		driver = createVMwareFusionHost(config)
	case "kvm":
		driver = createKVMHost(config)
	case "xhyve":
		driver = createXhyveHost(config)
	case "hyperv":
		driver = createHypervHost(config)
	default:
		glog.Exitf("Unsupported driver: %s\n", config.VMDriver)
	}

	data, err := json.Marshal(driver)
	if err != nil {
		return nil, err
	}

	h, err := api.NewHost(config.VMDriver, data)
	if err != nil {
		return nil, fmt.Errorf("Error creating new host: %s", err)
	}

	h.HostOptions.AuthOptions.CertDir = constants.Minipath
	h.HostOptions.AuthOptions.StorePath = constants.Minipath
	h.HostOptions.EngineOptions = &engine.Options{}

	if err := api.Create(h); err != nil {
		// Wait for all the logs to reach the client
		time.Sleep(2 * time.Second)
		return nil, fmt.Errorf("Error creating. %s", err)
	}

	if err := api.Save(h); err != nil {
		return nil, fmt.Errorf("Error attempting to save store: %s", err)
	}
	return h, nil
}

// GetHostDockerEnv gets the necessary docker env variables to allow the use of docker through minikube's vm
func GetHostDockerEnv(api libmachine.API) (map[string]string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return nil, err
	}
	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, err
	}

	tcpPrefix := "tcp://"
	portDelimiter := ":"
	port := "2376"

	envMap := map[string]string{
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_HOST":       tcpPrefix + ip + portDelimiter + port,
		"DOCKER_CERT_PATH":  constants.MakeMiniPath("certs"),
	}
	return envMap, nil
}

// GetHostLogs gets the openshift logs of the host VM.
func GetHostLogs(api libmachine.API) (string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}
	s, err := host.RunSSHCommand(logsCommand)
	if err != nil {
		return "", nil
	}
	return s, err
}

func checkIfApiExistsAndLoad(api libmachine.API) (*host.Host, error) {
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("Machine does not exist for api.Exists(%s)", constants.MachineName)
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return nil, err
	}
	return host, nil
}

func CreateSSHShell(api libmachine.API, args []string) error {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return err
	}

	currentState, err := host.Driver.GetState()
	if err != nil {
		return err
	}

	if currentState != state.Running {
		return fmt.Errorf("Error: Cannot run ssh command: Host %q is not running", constants.MachineName)
	}

	client, err := host.CreateSSHClient()
	if err != nil {
		return err
	}
	return client.Shell(strings.Join(args, " "))
}

func GetDashboardURL(api libmachine.API) (string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}

	ip, err := host.Driver.GetIP()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("https://%s:%d", ip, constants.APIServerPort), nil
}

func GetServiceURL(api libmachine.API, namespace, service string) (string, error) {
	host, err := checkIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}

	ip, err := host.Driver.GetIP()
	if err != nil {
		return "", err
	}

	port, err := getServicePort(namespace, service)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

type serviceGetter interface {
	Get(name string) (*kubeApi.Service, error)
}

func getServicePort(namespace, service string) (int, error) {
	services, err := getKubernetesServicesWithNamespace(namespace)
	if err != nil {
		return 0, err
	}
	return getServicePortFromServiceGetter(services, service)
}

func getServicePortFromServiceGetter(services serviceGetter, service string) (int, error) {
	svc, err := services.Get(service)
	if err != nil {
		return 0, fmt.Errorf("Error getting %s service: %s", service, err)
	}
	nodePort := 0
	if len(svc.Spec.Ports) > 0 {
		nodePort = int(svc.Spec.Ports[0].NodePort)
	}
	if nodePort == 0 {
		return 0, fmt.Errorf("Service %s does not have a node port. To have one assigned automatically, the service type must be NodePort or LoadBalancer, but this service is of type %s.", service, svc.Spec.Type)
	}
	return nodePort, nil
}

func getKubernetesServicesWithNamespace(namespace string) (serviceGetter, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("Error creating kubeConfig: %s", err)
	}
	client, err := unversioned.New(config)
	if err != nil {
		return nil, err
	}
	services := client.Services(namespace)
	return services, nil
}
