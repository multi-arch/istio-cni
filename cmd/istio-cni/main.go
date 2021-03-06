// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a sample chained plugin that supports multiple CNI versions. It
// parses prevResult according to the cniVersion
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/projectcalico/libcalico-go/lib/logutils"
	"github.com/sirupsen/logrus"
)

var (
	nsSetupBinDir = "/opt/cni/bin"
	nsSetupProg   = "istio-iptables.sh"

	injectAnnotationKey = "sidecar.istio.io/inject"
	sidecarStatusKey    = "sidecar.istio.io/status"
)

// setupRedirect is a unit test override variable.
var setupRedirect func(string, []string) error

// Kubernetes a K8s specific struct to hold config
type Kubernetes struct {
	K8sAPIRoot        string   `json:"k8s_api_root"`
	Kubeconfig        string   `json:"kubeconfig"`
	NodeName          string   `json:"node_name"`
	ExcludeNamespaces []string `json:"exclude_namespaces"`
	CniBinDir         string   `json:"cni_bin_dir"`
	IptablesScript    string   `json:"iptables_script"`
}

// PluginConf is whatever you expect your configuration json to be. This is whatever
// is passed in on stdin. Your plugin may wish to expose its functionality via
// runtime args, see CONVENTIONS.md in the CNI spec.
type PluginConf struct {
	types.NetConf // You may wish to not nest this type
	RuntimeConfig *struct {
		// SampleConfig map[string]interface{} `json:"sample"`
	} `json:"runtimeConfig"`

	// This is the previous result, when called in the context of a chained
	// plugin. Because this plugin supports multiple versions, we'll have to
	// parse this in two passes. If your plugin is not chained, this can be
	// removed (though you may wish to error if a non-chainable plugin is
	// chained.
	// If you need to modify the result before returning it, you will need
	// to actually convert it to a concrete versioned struct.
	RawPrevResult *map[string]interface{} `json:"prevResult"`
	PrevResult    *current.Result         `json:"-"`

	// Add plugin-specific flags here
	LogLevel   string     `json:"log_level"`
	Kubernetes Kubernetes `json:"kubernetes"`
}

// K8sArgs is the valid CNI_ARGS used for Kubernetes
// The field names need to match exact keys in kubelet args for unmarshalling
type K8sArgs struct {
	types.CommonArgs
	IP                         net.IP
	K8S_POD_NAME               types.UnmarshallableString // nolint: golint
	K8S_POD_NAMESPACE          types.UnmarshallableString // nolint: golint
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString // nolint: golint
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result. Remove this if your plugin is not chained.
	if conf.RawPrevResult != nil {
		resultBytes, err := json.Marshal(conf.RawPrevResult)
		if err != nil {
			return nil, fmt.Errorf("could not serialize prevResult: %v", err)
		}
		res, err := version.NewResult(conf.CNIVersion, resultBytes)
		if err != nil {
			return nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
		conf.RawPrevResult = nil
		conf.PrevResult, err = current.NewResultFromResult(res)
		if err != nil {
			return nil, fmt.Errorf("could not convert result to current version: %v", err)
		}
	}
	// End previous result parsing

	return &conf, nil
}

// ConfigureLogging sets up logging using the provided log level,
func ConfigureLogging(logLevel string) {
	if strings.EqualFold(logLevel, "debug") {
		logrus.SetLevel(logrus.DebugLevel)
	} else if strings.EqualFold(logLevel, "info") {
		logrus.SetLevel(logrus.InfoLevel)
	} else {
		// Default level
		logrus.SetLevel(logrus.WarnLevel)
	}

	logrus.SetOutput(os.Stderr)
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	logrus.Info("istio-cni cmdAdd parsing config")
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}
	ConfigureLogging(conf.LogLevel)

	var loggedPrevResult interface{}
	if conf.PrevResult == nil {
		loggedPrevResult = "none"
	} else {
		loggedPrevResult = conf.PrevResult
	}

	logrus.WithFields(logrus.Fields{
		"version":    conf.CNIVersion,
		"prevResult": loggedPrevResult,
	}).Info("cmdAdd config parsed")

	// Determine if running under k8s by checking the CNI args
	k8sArgs := K8sArgs{}
	if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
		return err
	}
	logrus.Infof("Getting identifiers with arguments: %s", args.Args)
	logrus.Infof("Loaded k8s arguments: %v", k8sArgs)
	if conf.Kubernetes.CniBinDir != "" {
		nsSetupBinDir = conf.Kubernetes.CniBinDir
	}
	if conf.Kubernetes.IptablesScript != "" {
		nsSetupProg = conf.Kubernetes.IptablesScript
	}

	podName := string(k8sArgs.K8S_POD_NAME)
	podNamespace := string(k8sArgs.K8S_POD_NAMESPACE)

	logger := logrus.WithFields(logrus.Fields{
		"ContainerID": args.ContainerID,
		"Pod":         podName,
		"Namespace":   podNamespace,
	})

	// Check if the workload is running under Kubernetes.
	if podNamespace != "" && podName != "" {
		excludePod := false
		for _, excludeNs := range conf.Kubernetes.ExcludeNamespaces {
			if podNamespace == excludeNs {
				excludePod = true
				break
			}
		}
		if !excludePod {
			client, err := newKubeClient(*conf, logger)
			if err != nil {
				return err
			}
			logrus.WithField("client", client).Debug("Created Kubernetes client")
			hasProxy, containers, _, annotations, ports, proxyUID, proxyGID, err := getKubePodInfo(client, podName, podNamespace)
			if err != nil {
				logger.Errorf("Error getting Pod data %v", err)
				return err
			}
			logger.Infof("Found containers %v", containers)
			if hasProxy && len(containers) > 1 {
				logrus.WithFields(logrus.Fields{
					"ContainerID": args.ContainerID,
					"netns":       args.Netns,
					"pod":         podName,
					"Namespace":   podNamespace,
					"ports":       ports,
					"annotations": annotations,
				}).Infof("Checking annotations prior to redirect for Istio proxy")

				if val, ok := annotations[injectAnnotationKey]; ok {
					logrus.Infof("Pod %s contains inject annotation: %s", podName, val)
					if injectEnabled, err := strconv.ParseBool(val); err == nil {
						if !injectEnabled {
							logrus.Infof("Pod excluded due to inject-disabled annotation")
							excludePod = true
						}
					}
				}
				if _, ok := annotations[sidecarStatusKey]; !ok {
					logrus.Infof("Pod %s excluded due to not containing sidecar annotation", podName)
					excludePod = true
				}
				if !excludePod {
					logrus.Infof("setting up redirect")
					if redirect, err := NewRedirect(proxyUID, proxyGID, ports, annotations, logger); err != nil {
						logger.Errorf("Pod redirect failed due to bad params: %v", err)
						return err
					} else {
						if setupRedirect != nil {
							_ = setupRedirect(args.Netns, ports)
						} else if err := redirect.doRedirect(args.Netns); err != nil {
							return err
						}
					}
				}
			}
		} else {
			logger.Infof("Pod excluded")
		}
	} else {
		logger.Infof("No Kubernetes Data")
	}

	var result *current.Result
	if conf.PrevResult == nil {
		result = &current.Result{
			CNIVersion: current.ImplementedSpecVersion,
		}
	} else {
		// Pass through the result for the next plugin
		result = conf.PrevResult
	}
	return types.PrintResult(result, conf.CNIVersion)
}

func cmdGet(args *skel.CmdArgs) error {
	logrus.Info("cmdGet not implemented")
	// TODO: implement
	return fmt.Errorf("not implemented")
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	logrus.Info("istio-cni cmdDel parsing config")
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}
	ConfigureLogging(conf.LogLevel)
	_ = conf

	// Do your delete here

	return nil
}

func main() {
	// Set up logging formatting.
	logrus.SetFormatter(&logutils.Formatter{})

	// Install a hook that adds file/line no information.
	logrus.AddHook(&logutils.ContextHook{})

	// TODO: implement plugin version
	skel.PluginMain(cmdAdd, cmdGet, cmdDel, version.All, "istio-cni")
}
