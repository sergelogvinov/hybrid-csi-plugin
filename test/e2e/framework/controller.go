//go:build e2e

/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// ControllerLocal runs the controller under test as a local process started by
	// the tests, with the flags each test asks for.
	ControllerLocal = "local"
	// ControllerExternal runs the tests against the controller deployed in the cluster.
	ControllerExternal = "external"

	controllerPackage = "github.com/sergelogvinov/hybrid-csi-plugin/cmd/csi-provisioner"

	// controllerSelector selects the controller pods of the helm chart. The Deployment
	// itself does not have the component label, only its pod template.
	controllerSelector = "app.kubernetes.io/name=hybrid-csi-plugin,app.kubernetes.io/component=controller"

	controllerRestartDelay = time.Second
	controllerStopTimeout  = 10 * time.Second
	controllerLogTail      = 50
)

// controllerWorkDir holds the controller binary and the pid file of the running controller.
var controllerWorkDir = filepath.Join(os.TempDir(), "hybrid-csi-e2e")

// Controller is a local controller process. Like the kubelet, it restarts the process
// when it exits until Stop is called, so crash tests see the real recovery path.
type Controller struct {
	t       *testing.T
	binary  string
	args    []string
	logPath string
	logFile *os.File

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopped  bool
	restarts int

	stop chan struct{}
	done chan struct{}
}

// RequireLocalController skips the test unless the controller runs locally: the test
// needs controller flags or crashes that a deployed controller does not have.
func RequireLocalController(t *testing.T) {
	t.Helper()

	if SharedConfig.Controller != ControllerLocal {
		t.Skipf("the test needs a local controller (E2E_CONTROLLER=%s)", ControllerLocal)
	}
}

// StartController starts a local controller with the default arguments, E2E_CONTROLLER_ARGS
// and args, and stops it on cleanup. Its log is written to the artifact directory of the test
// (go test -artifacts keeps it), and the tail of the log is printed when the test fails.
func StartController(t *testing.T, args ...string) (*Controller, error) {
	t.Helper()

	cfg := SharedConfig

	logPath := filepath.Join(t.ArtifactDir(), "controller.log")

	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create controller log: %w", err)
	}

	kubeconfig := cfg.Kubeconfig
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}

	c := &Controller{
		t:       t,
		binary:  cfg.ControllerBinary,
		args:    append(append([]string{"--kubeconfig=" + kubeconfig}, cfg.ControllerArgs...), args...),
		logPath: logPath,
		logFile: logFile,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	c.mu.Lock()
	err = c.start()
	c.mu.Unlock()

	if err != nil {
		logFile.Close() //nolint:errcheck

		return nil, err
	}

	t.Logf("[%s] started controller %v, log %s", time.Now().Format(time.TimeOnly), c.args[1:], logPath)

	go c.supervise()

	t.Cleanup(func() {
		c.Stop()

		if t.Failed() {
			c.logTail()
		}

		logFile.Close() //nolint:errcheck
	})

	return c, nil
}

// Restarts returns how many times the controller process exited and was restarted.
func (c *Controller) Restarts() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.restarts
}

// Stop terminates the controller and does not restart it. Safe to call more than once.
func (c *Controller) Stop() {
	c.mu.Lock()

	if c.stopped {
		c.mu.Unlock()
		<-c.done

		return
	}

	c.stopped = true
	cmd := c.cmd

	close(c.stop)
	c.mu.Unlock()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		c.t.Logf("failed to terminate controller: %v", err)
	}

	select {
	case <-c.done:
	case <-time.After(controllerStopTimeout):
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.t.Logf("failed to kill controller: %v", err)
		}

		<-c.done
	}

	removePIDFile()
}

// start launches the process, c.mu must be held.
func (c *Controller) start() error {
	// Not bound to a context: the process lives until Stop.
	cmd := exec.CommandContext(context.Background(), c.binary, c.args...) //nolint:gosec // the binary and args are set by the suite
	cmd.Stdout = c.logFile
	cmd.Stderr = c.logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start controller %s: %w", c.binary, err)
	}

	writePIDFile(cmd.Process.Pid)

	c.cmd = cmd

	return nil
}

func (c *Controller) supervise() {
	defer close(c.done)

	for {
		c.mu.Lock()
		cmd := c.cmd
		c.mu.Unlock()

		err := cmd.Wait()

		select {
		case <-c.stop:
			return
		default:
		}

		c.mu.Lock()
		c.restarts++
		restarts := c.restarts
		c.mu.Unlock()

		c.t.Logf("[%s] controller exited (%v), restart %d", time.Now().Format(time.TimeOnly), err, restarts)

		select {
		case <-c.stop:
			return
		case <-time.After(controllerRestartDelay):
		}

		c.mu.Lock()

		if c.stopped {
			c.mu.Unlock()

			return
		}

		err = c.start()
		c.mu.Unlock()

		if err != nil {
			c.t.Errorf("failed to restart controller: %v", err)

			return
		}
	}
}

// logTail prints the last lines of the controller log.
func (c *Controller) logTail() {
	f, err := os.Open(c.logPath)
	if err != nil {
		c.t.Logf("failed to read controller log: %v", err)

		return
	}
	defer f.Close() //nolint:errcheck

	lines := make([]string, 0, controllerLogTail)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if len(lines) == controllerLogTail {
			lines = lines[1:]
		}

		lines = append(lines, scanner.Text())
	}

	c.t.Logf("last %d lines of the controller log %s:\n%s", len(lines), c.logPath, strings.Join(lines, "\n"))
}

// setupLocalController prepares the local controller mode once per test binary: builds the
// controller unless E2E_CONTROLLER_BINARY is set, kills a controller left over by a killed
// run, and checks that no controller is deployed in the cluster, so only one controller
// handles the claims.
func setupLocalController(ctx context.Context, cfg *Config, clientset *kubernetes.Clientset) error {
	if err := os.MkdirAll(controllerWorkDir, 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", controllerWorkDir, err)
	}

	killLeftoverController(ctx)

	if cfg.ControllerBinary == "" {
		cfg.ControllerBinary = filepath.Join(controllerWorkDir, "hybrid-csi-provisioner")

		log.Printf("e2e: building the controller %s", cfg.ControllerBinary)

		build := exec.CommandContext(ctx, "go", "build", "-tags", "faultinject", "-o", cfg.ControllerBinary, controllerPackage)
		build.Stdout = os.Stderr
		build.Stderr = os.Stderr

		if err := build.Run(); err != nil {
			return fmt.Errorf("failed to build the controller: %w", err)
		}
	}

	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: controllerSelector})
	if err != nil {
		return fmt.Errorf("failed to list controller pods: %w", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		return fmt.Errorf("the controller is deployed in the cluster (pod %s/%s is %s): scale its deployment to 0, "+
			"or run against it with E2E_CONTROLLER=%s", pod.Namespace, pod.Name, pod.Status.Phase, ControllerExternal)
	}

	return nil
}

func pidFile() string {
	return filepath.Join(controllerWorkDir, "controller.pid")
}

func writePIDFile(pid int) {
	if err := os.WriteFile(pidFile(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		log.Printf("e2e: failed to write %s: %v", pidFile(), err)
	}
}

func removePIDFile() {
	if err := os.Remove(pidFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("e2e: failed to remove %s: %v", pidFile(), err)
	}
}

// killLeftoverController kills the controller of a run killed before its cleanup,
// e.g. by the go test timeout.
func killLeftoverController(ctx context.Context) {
	data, err := os.ReadFile(pidFile())
	if err != nil {
		return
	}

	defer removePIDFile()

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}

	// Only a process running our binary, the pid may have been reused.
	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil || !strings.Contains(string(out), "hybrid-csi-provisioner") {
		return
	}

	log.Printf("e2e: killing the controller %d left over by a previous run", pid)

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		log.Printf("e2e: failed to kill the controller %d: %v", pid, err)
	}
}
