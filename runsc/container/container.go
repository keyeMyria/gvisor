// Copyright 2018 Google Inc.
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

// Package container creates and manipulates containers.
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/pkg/sentry/control"
	"gvisor.googlesource.com/gvisor/runsc/boot"
	"gvisor.googlesource.com/gvisor/runsc/sandbox"
	"gvisor.googlesource.com/gvisor/runsc/specutils"
)

// metadataFilename is the name of the metadata file relative to the container
// root directory that holds sandbox metadata.
const metadataFilename = "meta.json"

// validateID validates the container id.
func validateID(id string) error {
	// See libcontainer/factory_linux.go.
	idRegex := regexp.MustCompile(`^[\w+-\.]+$`)
	if !idRegex.MatchString(id) {
		return fmt.Errorf("invalid container id: %v", id)
	}
	return nil
}

// Container represents a containerized application. When running, the
// container is associated with a single Sandbox.
//
// Container metadata can be saved and loaded to disk. Within a root directory,
// we maintain subdirectories for each container named with the container id.
// The container metadata is stored as a json within the container directory
// in a file named "meta.json". This metadata format is defined by us and is
// not part of the OCI spec.
//
// Containers must write their metadata files after any change to their internal
// states. The entire container directory is deleted when the container is
// destroyed.
type Container struct {
	// ID is the container ID.
	ID string `json:"id"`

	// Spec is the OCI runtime spec that configures this container.
	Spec *specs.Spec `json:"spec"`

	// BundleDir is the directory containing the container bundle.
	BundleDir string `json:"bundleDir"`

	// Root is the directory containing the container metadata file.
	Root string `json:"root"`

	// CreatedAt is the time the container was created.
	CreatedAt time.Time `json:"createdAt"`

	// Owner is the container owner.
	Owner string `json:"owner"`

	// ConsoleSocket is the path to a unix domain socket that will receive
	// the console FD. It is only used during create, so we don't need to
	// store it in the metadata.
	ConsoleSocket string `json:"-"`

	// Status is the current container Status.
	Status Status `json:"status"`

	// GoferPid is the pid of the gofer running along side the sandbox. May
	// be 0 if the gofer has been killed.
	GoferPid int `json:"goferPid"`

	// Sandbox is the sandbox this container is running in. It will be nil
	// if the container is not in state Running or Created.
	Sandbox *sandbox.Sandbox `json:"sandbox"`
}

// Load loads a container with the given id from a metadata file. id may be an
// abbreviation of the full container id, in which case Load loads the
// container to which id unambiguously refers to.
// Returns ErrNotExist if container doesn't exist.
func Load(rootDir, id string) (*Container, error) {
	log.Debugf("Load container %q %q", rootDir, id)
	if err := validateID(id); err != nil {
		return nil, fmt.Errorf("error validating id: %v", err)
	}

	cRoot, err := findContainerRoot(rootDir, id)
	if err != nil {
		// Preserve error so that callers can distinguish 'not found' errors.
		return nil, err
	}

	metaFile := filepath.Join(cRoot, metadataFilename)
	metaBytes, err := ioutil.ReadFile(metaFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Preserve error so that callers can distinguish 'not found' errors.
			return nil, err
		}
		return nil, fmt.Errorf("error reading container metadata file %q: %v", metaFile, err)
	}
	var c Container
	if err := json.Unmarshal(metaBytes, &c); err != nil {
		return nil, fmt.Errorf("error unmarshaling container metadata from %q: %v", metaFile, err)
	}

	// If the status is "Running" or "Created", check that the sandbox
	// process still exists, and set it to Stopped if it does not.
	//
	// This is inherently racey.
	if c.Status == Running || c.Status == Created {
		// Check if the sandbox process is still running.
		if !c.Sandbox.IsRunning() {
			// Sandbox no longer exists, so this container definitely does not exist.
			c.changeStatus(Stopped)
			c.Sandbox = nil
		} else if c.Status == Running {
			// Container state should reflect the actual state of
			// the application, so we don't consider gofer process
			// here.
			if err := c.Signal(syscall.Signal(0)); err != nil {
				c.changeStatus(Stopped)
			}
		}
	}

	return &c, nil
}

func findContainerRoot(rootDir, partialID string) (string, error) {
	// Check whether the id fully specifies an existing container.
	cRoot := filepath.Join(rootDir, partialID)
	if _, err := os.Stat(cRoot); err == nil {
		return cRoot, nil
	}

	// Now see whether id could be an abbreviation of exactly 1 of the
	// container ids. If id is ambigious (it could match more than 1
	// container), it is an error.
	cRoot = ""
	ids, err := List(rootDir)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if strings.HasPrefix(id, partialID) {
			if cRoot != "" {
				return "", fmt.Errorf("id %q is ambiguous and could refer to multiple containers: %q, %q", partialID, cRoot, id)
			}
			cRoot = id
		}
	}
	if cRoot == "" {
		return "", os.ErrNotExist
	}
	log.Debugf("abbreviated id %q resolves to full id %q", partialID, cRoot)
	return filepath.Join(rootDir, cRoot), nil
}

// List returns all container ids in the given root directory.
func List(rootDir string) ([]string, error) {
	log.Debugf("List containers %q", rootDir)
	fs, err := ioutil.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("ReadDir(%s) failed: %v", rootDir, err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Name())
	}
	return out, nil
}

// Create creates the container in a new Sandbox process, unless the metadata
// indicates that an existing Sandbox should be used. The caller must call
// Destroy() on the container.
func Create(id string, spec *specs.Spec, conf *boot.Config, bundleDir, consoleSocket, pidFile string) (*Container, error) {
	log.Debugf("Create container %q in root dir: %s", id, conf.RootDir)
	if err := validateID(id); err != nil {
		return nil, err
	}

	containerRoot := filepath.Join(conf.RootDir, id)
	if _, err := os.Stat(containerRoot); err == nil {
		return nil, fmt.Errorf("container with id %q already exists: %q", id, containerRoot)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("error looking for existing container in %q: %v", containerRoot, err)
	}

	c := &Container{
		ID:            id,
		Spec:          spec,
		ConsoleSocket: consoleSocket,
		BundleDir:     bundleDir,
		Root:          containerRoot,
		Status:        Creating,
		Owner:         os.Getenv("USER"),
	}

	// If the metadata annotations indicate that this container should be
	// started in an existing sandbox, we must do so. The metadata will
	// indicate the ID of the sandbox, which is the same as the ID of the
	// init container in the sandbox.
	if specutils.ShouldCreateSandbox(spec) || !conf.MultiContainer {
		log.Debugf("Creating new sandbox for container %q", id)
		ioFiles, err := c.createGoferProcess(spec, conf, bundleDir)
		if err != nil {
			return nil, err
		}

		// Start a new sandbox for this container. Any errors after this point
		// must destroy the container.
		s, err := sandbox.Create(id, spec, conf, bundleDir, consoleSocket, ioFiles)
		if err != nil {
			c.Destroy()
			return nil, err
		}
		c.Sandbox = s
	} else {
		// This is sort of confusing. For a sandbox with a root
		// container and a child container in it, runsc sees:
		// * A container struct whose sandbox ID is equal to the
		//   container ID. This is the root container that is tied to
		//   the creation of the sandbox.
		// * A container struct whose sandbox ID is equal to the above
		//   container/sandbox ID, but that has a different container
		//   ID. This is the child container.
		sbid, ok := specutils.SandboxID(spec)
		if !ok {
			return nil, fmt.Errorf("no sandbox ID found when creating container")
		}
		log.Debugf("Creating new container %q in sandbox %q", c.ID, sbid)

		// Find the sandbox associated with this ID.
		sb, err := Load(conf.RootDir, sbid)
		if err != nil {
			c.Destroy()
			return nil, err
		}
		c.Sandbox = sb.Sandbox
	}
	c.changeStatus(Created)

	// Save the metadata file.
	if err := c.save(); err != nil {
		c.Destroy()
		return nil, err
	}

	// Write the pid file. Containerd considers the create complete after
	// this file is created, so it must be the last thing we do.
	if pidFile != "" {
		if err := ioutil.WriteFile(pidFile, []byte(strconv.Itoa(c.Pid())), 0644); err != nil {
			c.Destroy()
			return nil, fmt.Errorf("error writing pid file: %v", err)
		}
	}

	return c, nil
}

// Start starts running the containerized process inside the sandbox.
func (c *Container) Start(conf *boot.Config) error {
	log.Debugf("Start container %q", c.ID)
	if c.Status != Created {
		return fmt.Errorf("cannot start container in state %s", c.Status)
	}

	// "If any prestart hook fails, the runtime MUST generate an error,
	// stop and destroy the container" -OCI spec.
	if c.Spec.Hooks != nil {
		if err := executeHooks(c.Spec.Hooks.Prestart, c.State()); err != nil {
			return err
		}
	}

	if specutils.ShouldCreateSandbox(c.Spec) || !conf.MultiContainer {
		if err := c.Sandbox.StartRoot(c.Spec, conf); err != nil {
			return err
		}
	} else {
		// Create the gofer process.
		ioFiles, err := c.createGoferProcess(c.Spec, conf, c.BundleDir)
		if err != nil {
			return err
		}
		if err := c.Sandbox.Start(c.Spec, conf, c.ID, ioFiles); err != nil {
			return err
		}
	}

	// "If any poststart hook fails, the runtime MUST log a warning, but
	// the remaining hooks and lifecycle continue as if the hook had
	// succeeded" -OCI spec.
	if c.Spec.Hooks != nil {
		executeHooksBestEffort(c.Spec.Hooks.Poststart, c.State())
	}

	c.changeStatus(Running)
	return c.save()
}

// Restore takes a container and replaces its kernel and file system
// to restore a container from its state file.
func (c *Container) Restore(spec *specs.Spec, conf *boot.Config, restoreFile string) error {
	log.Debugf("Restore container %q", c.ID)

	if c.Status != Created {
		return fmt.Errorf("cannot restore container in state %s", c.Status)
	}

	if err := c.Sandbox.Restore(c.ID, spec, conf, restoreFile); err != nil {
		return err
	}
	c.changeStatus(Running)
	return c.save()
}

// Run is a helper that calls Create + Start + Wait.
func Run(id string, spec *specs.Spec, conf *boot.Config, bundleDir, consoleSocket, pidFile string) (syscall.WaitStatus, error) {
	log.Debugf("Run container %q in root dir: %s", id, conf.RootDir)
	c, err := Create(id, spec, conf, bundleDir, consoleSocket, pidFile)
	if err != nil {
		return 0, fmt.Errorf("error creating container: %v", err)
	}
	defer c.Destroy()

	if err := c.Start(conf); err != nil {
		return 0, fmt.Errorf("error starting container: %v", err)
	}
	return c.Wait()
}

// Execute runs the specified command in the container. It returns the pid of
// the newly created process.
func (c *Container) Execute(args *control.ExecArgs) (int32, error) {
	log.Debugf("Execute in container %q, args: %+v", c.ID, args)
	if c.Status != Created && c.Status != Running {
		return 0, fmt.Errorf("cannot exec in container in state %s", c.Status)
	}
	return c.Sandbox.Execute(c.ID, args)
}

// Event returns events for the container.
func (c *Container) Event() (*boot.Event, error) {
	log.Debugf("Getting events for container %q", c.ID)
	if c.Status != Running && c.Status != Created {
		return nil, fmt.Errorf("cannot get events for container in state: %s", c.Status)
	}
	return c.Sandbox.Event(c.ID)
}

// Pid returns the Pid of the sandbox the container is running in, or -1 if the
// container is not running.
func (c *Container) Pid() int {
	if c.Status != Running && c.Status != Created && c.Status != Paused {
		return -1
	}
	return c.Sandbox.Pid
}

// Wait waits for the container to exit, and returns its WaitStatus.
// Call to wait on a stopped container is needed to retrieve the exit status
// and wait returns immediately.
func (c *Container) Wait() (syscall.WaitStatus, error) {
	log.Debugf("Wait on container %q", c.ID)
	if c.Sandbox == nil || !c.Sandbox.IsRunning() {
		return 0, fmt.Errorf("container sandbox is not running")
	}
	return c.Sandbox.Wait(c.ID)
}

// WaitRootPID waits for process 'pid' in the sandbox's PID namespace and
// returns its WaitStatus.
func (c *Container) WaitRootPID(pid int32, clearStatus bool) (syscall.WaitStatus, error) {
	log.Debugf("Wait on pid %d in sandbox %q", pid, c.Sandbox.ID)
	if c.Sandbox == nil || !c.Sandbox.IsRunning() {
		return 0, fmt.Errorf("container sandbox is not running")
	}
	return c.Sandbox.WaitPID(c.Sandbox.ID, pid, clearStatus)
}

// WaitPID waits for process 'pid' in the container's PID namespace and returns
// its WaitStatus.
func (c *Container) WaitPID(pid int32, clearStatus bool) (syscall.WaitStatus, error) {
	log.Debugf("Wait on pid %d in container %q", pid, c.ID)
	if c.Sandbox == nil || !c.Sandbox.IsRunning() {
		return 0, fmt.Errorf("container sandbox is not running")
	}
	return c.Sandbox.WaitPID(c.ID, pid, clearStatus)
}

// Signal sends the signal to the container.
// Signal returns an error if the container is already stopped.
// TODO: Distinguish different error types.
func (c *Container) Signal(sig syscall.Signal) error {
	log.Debugf("Signal container %q: %v", c.ID, sig)
	if c.Status == Stopped {
		return fmt.Errorf("container sandbox is stopped")
	}
	// TODO: Query the container for its state, then save it.
	return c.Sandbox.Signal(c.ID, sig)
}

// Checkpoint sends the checkpoint call to the container.
// The statefile will be written to f, the file at the specified image-path.
func (c *Container) Checkpoint(f *os.File) error {
	log.Debugf("Checkpoint container %q", c.ID)
	if c.Status == Stopped {
		return fmt.Errorf("container sandbox is stopped")
	}
	return c.Sandbox.Checkpoint(c.ID, f)
}

// Pause suspends the container and its kernel.
// The call only succeeds if the container's status is created or running.
func (c *Container) Pause() error {
	log.Debugf("Pausing container %q", c.ID)
	switch c.Status {
	case Created, Running:
		if err := c.Sandbox.Pause(c.ID); err != nil {
			return fmt.Errorf("error pausing container: %v", err)
		}
		c.changeStatus(Paused)
		return c.save()
	default:
		return fmt.Errorf("container %q not created or running, not pausing", c.ID)
	}
}

// Resume unpauses the container and its kernel.
// The call only succeeds if the container's status is paused.
func (c *Container) Resume() error {
	log.Debugf("Resuming container %q", c.ID)
	switch c.Status {
	case Paused:
		if err := c.Sandbox.Resume(c.ID); err != nil {
			return fmt.Errorf("error resuming container: %v", err)
		}
		c.changeStatus(Running)
		return c.save()
	default:
		return fmt.Errorf("container %q not paused, not resuming", c.ID)
	}
}

// State returns the metadata of the container.
func (c *Container) State() specs.State {
	return specs.State{
		Version: specs.Version,
		ID:      c.ID,
		Status:  c.Status.String(),
		Pid:     c.Pid(),
		Bundle:  c.BundleDir,
	}
}

// Processes retrieves the list of processes and associated metadata inside a
// container.
func (c *Container) Processes() ([]*control.Process, error) {
	if c.Status != Running && c.Status != Paused {
		return nil, fmt.Errorf("cannot get processes of container %q because it isn't running. It is in state %v", c.ID, c.Status)
	}
	return c.Sandbox.Processes(c.ID)
}

// Destroy frees all resources associated with the container. It fails fast and
// is idempotent.
func (c *Container) Destroy() error {
	log.Debugf("Destroy container %q", c.ID)

	if err := c.stop(); err != nil {
		return fmt.Errorf("error stopping container: %v", err)
	}

	if err := destroyFS(c.Spec); err != nil {
		return fmt.Errorf("error destroying container fs: %v", err)
	}

	if err := os.RemoveAll(c.Root); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("error deleting container root directory %q: %v", c.Root, err)
	}

	// "If any poststop hook fails, the runtime MUST log a warning, but the
	// remaining hooks and lifecycle continue as if the hook had succeeded" -OCI spec.
	// Based on the OCI, "The post-stop hooks MUST be called after the container is
	// deleted but before the delete operation returns"
	// Run it here to:
	// 1) Conform to the OCI.
	// 2) Make sure it only runs once, because the root has been deleted, the container
	// can't be loaded again.
	if c.Spec.Hooks != nil {
		executeHooksBestEffort(c.Spec.Hooks.Poststop, c.State())
	}

	c.changeStatus(Stopped)
	return nil
}

// save saves the container metadata to a file.
func (c *Container) save() error {
	log.Debugf("Save container %q", c.ID)
	if err := os.MkdirAll(c.Root, 0711); err != nil {
		return fmt.Errorf("error creating container root directory %q: %v", c.Root, err)
	}
	meta, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("error marshaling container metadata: %v", err)
	}
	metaFile := filepath.Join(c.Root, metadataFilename)
	if err := ioutil.WriteFile(metaFile, meta, 0640); err != nil {
		return fmt.Errorf("error writing container metadata: %v", err)
	}
	return nil
}

// stop stops the container (for regular containers) or the sandbox (for
// root containers), and waits for the container or sandbox and the gofer
// to stop. If any of them doesn't stop before timeout, an error is returned.
func (c *Container) stop() error {
	if c.Sandbox != nil && c.Sandbox.IsRunning() {
		log.Debugf("Destroying container %q", c.ID)
		if err := c.Sandbox.DestroyContainer(c.ID); err != nil {
			return fmt.Errorf("error destroying container %q: %v", c.ID, err)
		}
	}

	// Try killing gofer if it does not exit with container.
	if c.GoferPid != 0 {
		log.Debugf("Killing gofer for container %q, PID: %d", c.ID, c.GoferPid)
		if err := syscall.Kill(c.GoferPid, syscall.SIGKILL); err != nil {
			// The gofer may already be stopped, log the error.
			log.Warningf("Error sending signal %d to gofer %d: %v", syscall.SIGKILL, c.GoferPid, err)
		}
	}
	return c.waitForStopped()
}

func (c *Container) waitForStopped() error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	b := backoff.WithContext(backoff.NewConstantBackOff(100*time.Millisecond), ctx)
	op := func() error {
		if c.Sandbox != nil && c.Sandbox.IsRunning() {
			if err := c.Signal(syscall.Signal(0)); err == nil {
				return fmt.Errorf("container is still running")
			}
		}
		if c.GoferPid != 0 {
			if err := syscall.Kill(c.GoferPid, 0); err == nil {
				return fmt.Errorf("gofer is still running")
			}
			c.GoferPid = 0
		}
		return nil
	}
	return backoff.Retry(op, b)
}

func (c *Container) createGoferProcess(spec *specs.Spec, conf *boot.Config, bundleDir string) ([]*os.File, error) {
	if err := setupFS(spec, conf, bundleDir); err != nil {
		return nil, fmt.Errorf("failed to setup mounts: %v", err)
	}

	// Start with the general config flags.
	args := conf.ToFlags()
	args = append(args, "gofer", "--bundle", bundleDir)
	if conf.Overlay {
		args = append(args, "--panic-on-write=true")
	}

	// Add root mount and then add any other additional mounts.
	mountCount := 1

	// Add additional mounts.
	for _, m := range spec.Mounts {
		if specutils.Is9PMount(m) {
			mountCount++
		}
	}
	sandEnds := make([]*os.File, 0, mountCount)
	goferEnds := make([]*os.File, 0, mountCount)

	// nextFD is the next available file descriptor for the gofer process.
	// It starts at 3 because 0-2 are used by stdin/stdout/stderr.
	nextFD := 3
	for ; nextFD-3 < mountCount; nextFD++ {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sandEnds = append(sandEnds, os.NewFile(uintptr(fds[0]), "sandbox io fd"))

		goferEnd := os.NewFile(uintptr(fds[1]), "gofer io fd")
		defer goferEnd.Close()
		goferEnds = append(goferEnds, goferEnd)

		args = append(args, fmt.Sprintf("--io-fds=%d", nextFD))
	}

	binPath, err := specutils.BinPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binPath, args...)
	cmd.ExtraFiles = goferEnds

	// Setup any uid/gid mappings, and create or join the configured user
	// namespace so the gofer's view of the filesystem aligns with the
	// users in the sandbox.
	specutils.SetUIDGIDMappings(cmd, spec)
	nss := specutils.FilterNS([]specs.LinuxNamespaceType{specs.UserNamespace}, spec)

	// Start the gofer in the given namespace.
	log.Debugf("Starting gofer: %s %v", binPath, args)
	if err := specutils.StartInNS(cmd, nss); err != nil {
		return nil, err
	}
	log.Infof("Gofer started, pid: %d", cmd.Process.Pid)
	c.GoferPid = cmd.Process.Pid
	return sandEnds, nil
}

// changeStatus transitions from one status to another ensuring that the
// transition is valid.
func (c *Container) changeStatus(s Status) {
	switch s {
	case Creating:
		// Initial state, never transitions to it.
		panic(fmt.Sprintf("invalid state transition: %v => %v", c.Status, s))

	case Created:
		if c.Status != Creating {
			panic(fmt.Sprintf("invalid state transition: %v => %v", c.Status, s))
		}
		if c.Sandbox == nil {
			panic("sandbox cannot be nil")
		}

	case Paused:
		if c.Status != Running {
			panic(fmt.Sprintf("invalid state transition: %v => %v", c.Status, s))
		}
		if c.Sandbox == nil {
			panic("sandbox cannot be nil")
		}

	case Running:
		if c.Status != Created && c.Status != Paused {
			panic(fmt.Sprintf("invalid state transition: %v => %v", c.Status, s))
		}
		if c.Sandbox == nil {
			panic("sandbox cannot be nil")
		}

	case Stopped:
		if c.Status != Created && c.Status != Running && c.Status != Stopped {
			panic(fmt.Sprintf("invalid state transition: %v => %v", c.Status, s))
		}

	default:
		panic(fmt.Sprintf("invalid new state: %v", s))
	}
	c.Status = s
}
