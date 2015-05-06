package qmd

import (
	"bytes"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Cmd struct {
	*exec.Cmd `json:"cmd"`

	JobID       string
	State       CmdState      `json:"state"`
	StartTime   time.Time     `json:"start_time,omitempty"`
	EndTime     time.Time     `json:"end_time,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
	StatusCode  int           `json:"status_code,omitempty"`
	CallbackURL string        `json:"callback_url"`
	Err         error         `json:"err,omitempty"`
	Priority    Priority      `json:"priority"`

	CmdOut bytes.Buffer `json:"-"`
	//QmdOut bytes.Buffer `json:"-"`
	QmdOutFile string `json:"qmdoutfile"`

	StoreDir          string            `json:"storedir"`
	ExtraWorkDirFiles map[string]string `json:"extraworkdirfiles"`

	// Started channel block until the cmd is started.
	Started chan struct{} `json:"-"`
	// Finished channel block until the cmd is finished/killed/invalidated.
	Finished chan struct{} `json:"-"`

	// WaitOnce guards the Wait() logic, so it can be called multiple times.
	WaitOnce sync.Once `json:"-"`
	// StartOnce guards the Start() logic, so it can be called multiple times.
	StartOnce sync.Once `json:"-"`
}

type CmdState int

const (
	Initialized CmdState = iota
	Running
	Finished
	Terminated
	Invalidated
	Failed
)

type Priority int

const (
	PriorityLow Priority = iota
	PriorityHigh
	PriorityUrgent
)

func (s Priority) String() string {
	switch s {
	case PriorityLow:
		return "low"
	case PriorityHigh:
		return "high"
	case PriorityUrgent:
		return "urgent"
	}
	panic("unreachable")
}

func (qmd *Qmd) Cmd(from *exec.Cmd) (*Cmd, error) {
	cmd := &Cmd{
		Cmd:      from,
		State:    Initialized,
		Started:  make(chan struct{}),
		Finished: make(chan struct{}),
		StoreDir: qmd.Config.StoreDir,
	}
	//TODO: Create random temp dir instead.
	cmd.Cmd.Dir = qmd.Config.WorkDir + "/" + cmd.JobID

	return cmd, nil
}

func (cmd *Cmd) Start() error {
	cmd.StartOnce.Do(cmd.startOnce)

	// Wait for cmd to start.
	<-cmd.Started

	return cmd.Err
}

func (cmd *Cmd) startOnce() {
	log.Printf("Cmd: Starting %v", cmd.JobID)

	cmd.QmdOutFile = cmd.Cmd.Dir + "/QMD_OUT"
	cmd.Cmd.Env = append(os.Environ(),
		"QMD_TMP="+cmd.Cmd.Dir,
		"QMD_STORE="+cmd.StoreDir,
		"QMD_OUT="+cmd.QmdOutFile,
	)

	cmd.Cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		//TODO: Chroot: cmd.Cmd.Dir,
	}

	cmd.Cmd.Stdout = &cmd.CmdOut
	cmd.Cmd.Stderr = &cmd.CmdOut

	// r, w, err := os.Pipe()
	// if err != nil {
	// 	cmd.Err = err
	// 	goto failedToStart
	// }
	// cmd.Cmd.ExtraFiles = []*os.File{w}
	// go cmd.QmdOut.ReadFrom(r)

	// Create working directory.
	err := os.MkdirAll(cmd.Cmd.Dir, 0777)
	if err != nil {
		cmd.Err = err
	}

	// Create QMD_OUT file.
	// TODO: Change this to pipe?
	qmdOut, err := os.Create(cmd.QmdOutFile)
	if err != nil {
		cmd.Err = err
	}
	qmdOut.Close()

	for file, data := range cmd.ExtraWorkDirFiles {
		// Must be a simple filename without slashes.
		if strings.Index(file, "/") != -1 {
			cmd.Err = errors.New("extra file contains slashes")
			goto failedToStart
		}
		err = ioutil.WriteFile(cmd.Cmd.Dir+"/tmp/"+file, []byte(data), 0644)
		if err != nil {
			cmd.Err = err
			goto failedToStart
		}
	}

	if err := cmd.Cmd.Start(); err != nil {
		cmd.Err = err
		goto failedToStart
	}

	cmd.StartTime = time.Now()
	cmd.State = Running
	close(cmd.Started)
	cmd.Err = nil
	return

failedToStart:
	cmd.StatusCode = -1
	cmd.State = Failed
	cmd.WaitOnce.Do(func() {
		close(cmd.Finished)
	})
	close(cmd.Started)
	log.Printf("Cmd: Failed to start %v: %v", cmd.JobID, cmd.Err)
}

// Wait waits for cmd to finish.
// It closes the Stdout and Stderr pipes.
func (cmd *Cmd) Wait() error {
	// Wait for Start(), if not already invoked.
	<-cmd.Started

	// Prevent running cmd.Wait() multiple times.
	cmd.WaitOnce.Do(cmd.waitOnce)

	// Wait for cmd to finish.
	<-cmd.Finished

	return cmd.Err
}

func (cmd *Cmd) waitOnce() {
	err := cmd.Cmd.Wait()
	cmd.Duration = time.Since(cmd.StartTime)
	cmd.EndTime = cmd.StartTime.Add(cmd.Duration)
	if cmd.State != Terminated {
		cmd.State = Finished
	}

	if err != nil {
		cmd.Err = err
		if e, ok := err.(*exec.ExitError); ok {
			if s, ok := e.Sys().(syscall.WaitStatus); ok {
				cmd.StatusCode = s.ExitStatus()
			}
		}
	}

	// Make sure to kill the whole process group,
	// so there are no subprocesses left.
	cmd.Kill()

	close(cmd.Finished)
	log.Printf("Cmd %v finished", cmd.JobID)
}

func (cmd *Cmd) Run() error {
	err := cmd.Start()
	if err != nil {
		return err
	}

	err = cmd.Wait()
	if err != nil {
		return err
	}

	return nil
}

func (cmd *Cmd) Kill() error {
	switch cmd.State {
	case Running:
		cmd.State = Terminated
		log.Printf("Cmd: Killing %v\n", cmd.JobID)
		pgid, err := syscall.Getpgid(cmd.Cmd.Process.Pid)
		if err != nil {
			// Fall-back on error. Kill the main process only.
			cmd.Cmd.Process.Kill()
			break
		}
		// Kill the whole process group.
		syscall.Kill(-pgid, 15)

	case Finished:
		log.Printf("Cmd: Killing (group) %v\n", cmd.JobID)
		pgid, err := syscall.Getpgid(cmd.Cmd.Process.Pid)
		if err != nil {
			break
		}
		// Make sure to kill the whole process group,
		// so there are no subprocesses left.
		syscall.Kill(-pgid, 15)

	case Initialized:
		// This one is tricky, as the cmd's Start() might have
		// been called and is already in progress, but the cmd's
		// state is not Running yet.
		usCallingStartOnce := false
		cmd.StartOnce.Do(func() {
			cmd.WaitOnce.Do(func() {
				cmd.State = Invalidated
				cmd.StatusCode = -2
				cmd.Err = errors.New("invalidated")
				log.Printf("Cmd: Invalidating %v\n", cmd.JobID)
				close(cmd.Finished)
			})
			close(cmd.Started)
			usCallingStartOnce = true
		})
		if !usCallingStartOnce {
			// It was cmd.Start() that called StartOnce.Do(), not us,
			// thus we need to wait for Started and try to Kill again:
			<-cmd.Started
			cmd.Kill()
		}
	}

	return cmd.Err
}

func (s CmdState) String() string {
	switch s {
	case Initialized:
		return "Initialized"
	case Running:
		return "Running"
	case Finished:
		return "Finished"
	case Terminated:
		return "Terminated (killed by us)"
	case Invalidated:
		return "Invalidated before start"
	case Failed:
		return "Failed to start"
	}
	panic("unreachable")
}
