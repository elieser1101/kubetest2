/*
Copyright 2021 The Kubernetes Authors.

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

// Package node implements a node tester that implements e2e node testing following
// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-node/e2e-node-tests.md#delete-instance-after-tests-run
// https://github.com/kubernetes/kubernetes/blob/96be00df69390ed41b8ec22facc43bcbb9c88aae/build/root/Makefile#L206-L271
// currently only support REMOTE=true
package node

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/octago/sflags/gen/gpflag"
	"k8s.io/klog/v2"

	"sigs.k8s.io/boskos/client"
	"sigs.k8s.io/kubetest2/pkg/boskos"
	"sigs.k8s.io/kubetest2/pkg/exec"
	"sigs.k8s.io/kubetest2/pkg/fs"
	"sigs.k8s.io/kubetest2/pkg/testers"
)

var GitTag string

const (
	target          = "test-e2e-node"
	ciPrivateKeyEnv = "GCE_SSH_PRIVATE_KEY_FILE"
	ciPublicKeyEnv  = "GCE_SSH_PUBLIC_KEY_FILE"
)

type Tester struct {
	RepoRoot                       string        `desc:"Absolute path to the kubernetes or provider-aws-test-infra repository root."`
	GCPProject                     string        `desc:"GCP Project to create VMs in. If unset, the deployer will attempt to get a project from boskos."`
	GCPZone                        string        `desc:"GCP Zone to create VMs in."`
	SkipRegex                      string        `desc:"Regular expression of jobs to skip."`
	FocusRegex                     string        `desc:"Regular expression of jobs to focus on."`
	ContainerRuntimeEndpoint       string        `desc:"remote container endpoint to connect to. Defaults to containerd"`
	TestArgs                       string        `desc:"A space-separated list of arguments to pass to node e2e test."`
	BoskosAcquireTimeoutSeconds    int           `desc:"How long (in seconds) to hang on a request to Boskos to acquire a resource before erroring."`
	BoskosHeartbeatIntervalSeconds int           `desc:"How often (in seconds) to send a heartbeat to Boskos to hold the acquired resource. 0 means no heartbeat."`
	BoskosLocation                 string        `desc:"If set, manually specifies the location of the boskos server. If unset and boskos is needed"`
	ImageConfigFile                string        `desc:"Path to a file containing image configuration."`
	Images                         string        `desc:"List of images to use when creating instances separated by commas"`
	ImageProject                   string        `desc:"A GCP Project containing an image to use when creating instances"`
	InstanceType                   string        `desc:"Machine/Instance type to use on AWS/GCP"`
	InstanceMetadata               string        `desc:"Instance Metadata to use for creating GCE instance"`
	UserDataFile                   string        `desc:"User Data to use for creating EC2 instance"`
	Provider                       string        `desc:"Cloud Provider to use for node tests. Valid options are ec2 and gce"`
	UseDockerizedBuild             bool          `desc:"Use dockerized build for test artifacts"`
	TargetBuildArch                string        `desc:"Target architecture for the test artifacts for dockerized build"`
	ImageConfigDir                 string        `desc:"Path to image config files."`
	Parallelism                    int           `desc:"The number of nodes to run in parallel."`
	GCPProjectType                 string        `desc:"Explicitly indicate which project type to select from boskos."`
	RuntimeConfig                  string        `desc:"The runtime configuration for the API server. Format: a list of key=value pairs."`
	Timeout                        time.Duration `desc:"How long (in golang duration format) to wait for ginkgo tests to complete."`
	DeleteInstances                bool          `desc:"Where to delete instances after running the test"`
	NodeEnv                        string        `desc:"Additional metadata keys to add to a gce instance"`

	// boskos struct field will be non-nil when the deployer is
	// using boskos to acquire a GCP project
	boskos *client.Client

	// this channel serves as a signal channel for the hearbeat goroutine
	// so that it can be explicitly closed
	boskosHeartbeatClose chan struct{}

	// this contains ssh key path
	privateKey string
	sshUser    string
}

func NewDefaultTester() *Tester {
	return &Tester{
		SkipRegex:                      `\[Flaky\]|\[Slow\]|\[Serial\]`,
		BoskosLocation:                 "http://boskos.test-pods.svc.cluster.local.",
		BoskosAcquireTimeoutSeconds:    5 * 60,
		BoskosHeartbeatIntervalSeconds: 5 * 60,
		Parallelism:                    8,
		boskosHeartbeatClose:           make(chan struct{}),
		GCPProjectType:                 "gce-project",
		Provider:                       "gce",
		DeleteInstances:                true,
	}
}

func (t *Tester) Execute() error {
	fs, err := gpflag.Parse(t)
	if err != nil {
		return fmt.Errorf("failed to initialize tester: %v", err)
	}

	// initing the klog flags adds them to goflag.CommandLine
	// they can then be added to the built pflag set
	klog.InitFlags(nil)
	fs.AddGoFlagSet(flag.CommandLine)

	help := fs.BoolP("help", "h", false, "")
	if err := fs.Parse(os.Args); err != nil {
		return fmt.Errorf("failed to parse flags: %v", err)
	}

	if *help {
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
		return nil
	}
	if err := t.validateFlags(); err != nil {
		return fmt.Errorf("failed to validate flags: %v", err)
	}

	// Use the KUBE_SSH_USER environment variable if it is set. This is particularly
	// required for Fedora CoreOS hosts that only have the user 'core`. Tests
	// using Fedora CoreOS as a host for node tests must set KUBE_SSH_USER
	// environment variable so that test infrastructure can communicate with the host
	// successfully using ssh.
	if os.Getenv("KUBE_SSH_USER") != "" {
		t.sshUser = os.Getenv("KUBE_SSH_USER")
	} else {
		t.sshUser = os.Getenv("USER")
	}

	if t.Provider == "gce" {
		t.maybeSetupSSHKeys()

		// try to acquire project from boskos
		if t.GCPProject == "" {
			klog.V(1).Info("no GCP project provided, acquiring from Boskos ...")

			boskosClient, err := boskos.NewClient(t.BoskosLocation)
			if err != nil {
				return fmt.Errorf("failed to make boskos client: %s", err)
			}
			t.boskos = boskosClient

			resource, err := boskos.Acquire(
				t.boskos,
				t.GCPProjectType,
				time.Duration(t.BoskosAcquireTimeoutSeconds)*time.Second,
				time.Duration(t.BoskosHeartbeatIntervalSeconds)*time.Second,
				t.boskosHeartbeatClose,
			)

			if err != nil {
				return fmt.Errorf("init failed to get project from boskos: %s", err)
			}
			t.GCPProject = resource.Name
			klog.V(1).Infof("got project %s from boskos", t.GCPProject)
		}
	}

	defer func() {
		if t.boskos != nil {
			klog.V(1).Info("releasing boskos project")
			err := boskos.Release(
				t.boskos,
				[]string{t.GCPProject},
				t.boskosHeartbeatClose,
			)
			if err != nil {
				klog.Errorf("failed to release boskos project: %v", err)
			}
		}
	}()
	if err := testers.WriteVersionToMetadata(GitTag); err != nil {
		return err
	}
	return t.Test()
}

func (t *Tester) validateFlags() error {
	if t.RepoRoot == "" {
		return fmt.Errorf("required --repo-root")
	}
	if t.GCPZone == "" && t.Provider == "gce" {
		return fmt.Errorf("required --gcp-zone")
	}
	return nil
}

// maybeSetupSSHKeys will best-effort try to setup ssh keys for gcloud to reuse
// from existing files pointed to by "well-known" environment variables used in CI
func (t *Tester) maybeSetupSSHKeys() {
	home, err := os.UserHomeDir()
	if err != nil {
		klog.Warningf("failed to get user's home directory")
		return
	}
	// check if there are existing ssh keys, if either exist don't do anything
	klog.V(2).Info("checking for existing gcloud ssh keys...")
	t.privateKey = filepath.Join(home, ".ssh", "google_compute_engine")
	if _, err := os.Stat(t.privateKey); err == nil {
		klog.V(2).Infof("found existing private key at %s", t.privateKey)
		return
	}
	publicKey := t.privateKey + ".pub"
	if _, err := os.Stat(publicKey); err == nil {
		klog.V(2).Infof("found existing public key at %s", publicKey)
		return
	}

	// no existing keys check for CI variables, create gcloud key files if both exist
	// note only checks if relevant envs are non-empty, no actual key verification checks
	maybePrivateKey, privateKeyEnvSet := os.LookupEnv(ciPrivateKeyEnv)
	if !privateKeyEnvSet {
		klog.V(2).Infof("%s is not set", ciPrivateKeyEnv)
		return
	}
	maybePublicKey, publicKeyEnvSet := os.LookupEnv(ciPublicKeyEnv)
	if !publicKeyEnvSet {
		klog.V(2).Infof("%s is not set", ciPublicKeyEnv)
		return
	}

	if err := fs.CopyFile(maybePrivateKey, t.privateKey); err != nil {
		klog.Warningf("failed to copy %s to %s: %v", maybePrivateKey, t.privateKey, err)
		return
	}

	if err := fs.CopyFile(maybePublicKey, publicKey); err != nil {
		klog.Warningf("failed to copy %s to %s: %v", maybePublicKey, publicKey, err)
	}
}

func (t *Tester) constructArgs() []string {
	defaultArgs := []string{
		"REMOTE=true",
	}

	argsFromFlags := []string{
		"SKIP=" + t.SkipRegex,
		"FOCUS=" + t.FocusRegex,
		// https://github.com/kubernetes/kubernetes/blob/96be00df69390ed41b8ec22facc43bcbb9c88aae/hack/make-rules/test-e2e-node.sh#L120
		// TODO: this should be configurable without overriding at the gcloud env level
		"CLOUDSDK_CORE_PROJECT=" + t.GCPProject,
		// https://github.com/kubernetes/kubernetes/blob/96be00df69390ed41b8ec22facc43bcbb9c88aae/hack/make-rules/test-e2e-node.sh#L113
		"ZONE=" + t.GCPZone,
		"CONTAINER_RUNTIME_ENDPOINT=" + t.ContainerRuntimeEndpoint,
		"TEST_ARGS=" + t.TestArgs,
		"NODE_ENV= " + t.NodeEnv,
		"DELETE_INSTANCES=" + strconv.FormatBool(t.DeleteInstances),
		"PARALLELISM=" + strconv.Itoa(t.Parallelism),
		"IMAGE_CONFIG_FILE=" + t.ImageConfigFile,
		"IMAGE_CONFIG_DIR=" + t.ImageConfigDir,
		"IMAGE_PROJECT=" + t.ImageProject,
		"IMAGES=" + t.Images,
		"INSTANCE_METADATA=" + t.InstanceMetadata,
		"USER_DATA_FILE=" + t.UserDataFile,
		"INSTANCE_TYPE=" + t.InstanceType,
		"SSH_USER=" + t.sshUser,
		"SSH_KEY=" + t.privateKey,
		"USE_DOCKERIZED_BUILD=" + strconv.FormatBool(t.UseDockerizedBuild),
		"TARGET_BUILD_ARCH=" + t.TargetBuildArch,
		"TIMEOUT=" + t.Timeout.String(),
	}
	if t.RuntimeConfig != "" {
		argsFromFlags = append(argsFromFlags, "RUNTIME_CONFIG="+t.RuntimeConfig)
	}
	return append(defaultArgs, argsFromFlags...)
}

func (t *Tester) Test() error {
	var args []string
	args = append(args, target)
	args = append(args, t.constructArgs()...)
	cmd := exec.Command("make", args...)
	cmd.SetDir(t.RepoRoot)
	exec.InheritOutput(cmd)
	return cmd.Run()
}

func Main() {
	t := NewDefaultTester()
	if err := t.Execute(); err != nil {
		klog.Fatalf("failed to run ginkgo tester: %v", err)
	}
}
