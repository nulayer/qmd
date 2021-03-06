package qmd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/goware/disque"
	"github.com/goware/lg"
	"github.com/pressly/qmd/rest/api"
)

type Worker chan *disque.Job

func (qmd *Qmd) StartWorkers() {
	lg.Debugf("Starting %v QMD workers", qmd.Config.MaxJobs)
	for i := 0; i < qmd.Config.MaxJobs; i++ {
		go qmd.startWorker(i, qmd.Workers)
	}
}

func (qmd *Qmd) startWorker(id int, workers chan Worker) {
	qmd.WaitWorkers.Add(1)
	defer qmd.WaitWorkers.Done()

	worker := make(Worker)
	for {
		// Mark this worker as available.
		workers <- worker

		select {
		// Wait for a job.
		case job := <-worker:
			msg := fmt.Errorf("Worker %v:\tGot \"%v\" job %v/jobs/%v", id, job.Queue, qmd.Config.URL, job.ID)
			lg.Error(msg)
			qmd.Slack.Notify(msg.Error())

			var req *api.ScriptsRequest
			err := json.Unmarshal([]byte(job.Data), &req)
			if err != nil {
				qmd.Queue.Ack(job)
				msg := fmt.Errorf("Worker %v:\tfailed: %v", err)
				lg.Error(msg)
				qmd.Slack.Notify(msg.Error())
				break
			}

			script, err := qmd.GetScript(req.Script)
			if err != nil {
				qmd.Queue.Ack(job)
				msg := fmt.Errorf("Worker %v:\tfailed: %v", err)
				lg.Error(msg)
				qmd.Slack.Notify(msg.Error())
				break
			}

			// Create QMD job to run the command.
			cmd, err := qmd.Cmd(exec.Command(script, req.Args...))
			if err != nil {
				qmd.Queue.Ack(job)
				msg := fmt.Errorf("Worker %v:\tfailed: %v", err)
				lg.Error(msg)
				qmd.Slack.Notify(msg.Error())
				break
			}
			cmd.JobID = job.ID
			cmd.CallbackURL = req.CallbackURL
			cmd.ExtraWorkDirFiles = req.Files

			// Run a job.
			go cmd.Run()
			<-cmd.Started

			select {
			// Wait for the job to finish.
			case <-cmd.Finished:

			// Or kill it, if it doesn't finish in a specified time.
			case <-time.After(time.Duration(qmd.Config.MaxExecTime) * time.Second):
				cmd.Kill()
				cmd.Wait()
				cmd.Cleanup()

			// Or kill it, if QMD is closing.
			case <-qmd.ClosingWorkers:
				lg.Debugf("Worker %d:\tStopping (busy)", id)
				cmd.Kill()
				cmd.Cleanup()
				qmd.Queue.Nack(job)
				msg := fmt.Errorf("Worker %d:\tNACKed job %v/jobs/%v", id, qmd.Config.URL, job.ID)
				lg.Error(msg)
				qmd.Slack.Notify(msg.Error())
				return
			}

			// Response.
			resp := api.ScriptsResponse{
				ID:     job.ID,
				Script: req.Script,
				Args:   req.Args,
				Files:  req.Files,
			}

			// "OK" and "ERR" for backward compatibility.
			if cmd.StatusCode == 0 {
				resp.Status = "OK"
			} else {
				resp.Status = "ERR"
			}

			resp.EndTime = cmd.EndTime
			resp.Duration = fmt.Sprintf("%f", cmd.Duration.Seconds())
			resp.QmdOut = cmd.QmdOut.String()
			resp.ExecLog = cmd.CmdOut.String()
			resp.StartTime = cmd.StartTime
			if cmd.Err != nil {
				resp.Err = cmd.Err.Error()
			}

			qmd.DB.SaveResponse(&resp)

			qmd.Queue.Ack(job)
			msg = fmt.Errorf("Worker %v:\tACKed job %v/jobs/%v", id, qmd.Config.URL, job.ID)
			lg.Error(msg)
			qmd.Slack.Notify(msg.Error())

		case <-qmd.ClosingWorkers:
			lg.Debugf("Worker %d:\tStopping (idle)", id)
			return
		}
	}
}
