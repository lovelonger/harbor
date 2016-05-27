package job

import (
	"fmt"
	"sync"

	"github.com/vmware/harbor/dao"
	"github.com/vmware/harbor/job/config"
	"github.com/vmware/harbor/job/replication"
	"github.com/vmware/harbor/job/utils"
	"github.com/vmware/harbor/models"
	uti "github.com/vmware/harbor/utils"
	"github.com/vmware/harbor/utils/log"
)

type RepJobParm struct {
	LocalRegURL    string
	TargetURL      string
	TargetUsername string
	TargetPassword string
	Repository     string
	Tags           []string
	Enabled        int
	Operation      string
}

type JobSM struct {
	JobID         int64
	CurrentState  string
	PreviousState string
	//The states that don't have to exist in transition map, such as "Error", "Canceled"
	ForcedStates map[string]struct{}
	Transitions  map[string]map[string]struct{}
	Handlers     map[string]StateHandler
	desiredState string
	Logger       *log.Logger
	Parms        *RepJobParm
	lock         *sync.Mutex
}

// EnsterState transit the statemachine from the current state to the state in parameter.
// It returns the next state the statemachine should tranit to.
func (sm *JobSM) EnterState(s string) (string, error) {
	log.Debugf("Job id: %d, transiting from State: %s, to State: %s", sm.JobID, sm.CurrentState, s)
	targets, ok := sm.Transitions[sm.CurrentState]
	_, exist := targets[s]
	_, isForced := sm.ForcedStates[s]
	if !exist && !isForced {
		return "", fmt.Errorf("Job id: %d, transition from %s to %s does not exist!", sm.JobID, sm.CurrentState, s)
	}
	exitHandler, ok := sm.Handlers[sm.CurrentState]
	if ok {
		if err := exitHandler.Exit(); err != nil {
			return "", err
		}
	} else {
		log.Debugf("Job id: %d, no handler found for state:%s, skip", sm.JobID, sm.CurrentState)
	}
	enterHandler, ok := sm.Handlers[s]
	var next string = models.JobContinue
	var err error
	if ok {
		if next, err = enterHandler.Enter(); err != nil {
			return "", err
		}
	} else {
		log.Debugf("Job id: %d, no handler found for state:%s, skip", sm.JobID, s)
	}
	sm.PreviousState = sm.CurrentState
	sm.CurrentState = s
	log.Debugf("Job id: %d, transition succeeded, current state: %s", sm.JobID, s)
	return next, nil
}

// Start kicks off the statemachine to transit from current state to s, and moves on
// It will search the transit map if the next state is "_continue", and
// will enter error state if there's more than one possible path when next state is "_continue"
func (sm *JobSM) Start(s string) {
	n, err := sm.EnterState(s)
	log.Debugf("Job id: %d, next state from handler: %s", sm.JobID, n)
	for len(n) > 0 && err == nil {
		if d := sm.getDesiredState(); len(d) > 0 {
			log.Debugf("Job id: %d. Desired state: %s, will ignore the next state from handler", sm.JobID, d)
			n = d
			sm.setDesiredState("")
			continue
		}
		if n == models.JobContinue && len(sm.Transitions[sm.CurrentState]) == 1 {
			for n = range sm.Transitions[sm.CurrentState] {
				break
			}
			log.Debugf("Job id: %d, Continue to state: %s", sm.JobID, n)
			continue
		}
		if n == models.JobContinue && len(sm.Transitions[sm.CurrentState]) != 1 {
			log.Errorf("Job id: %d, next state is continue but there are %d possible next states in transition table", sm.JobID, len(sm.Transitions[sm.CurrentState]))
			err = fmt.Errorf("Unable to continue")
			break
		}
		n, err = sm.EnterState(n)
		log.Debugf("Job id: %d, next state from handler: %s", sm.JobID, n)
	}
	if err != nil {
		log.Warningf("Job id: %d, the statemachin will enter error state due to error: %v", sm.JobID, err)
		sm.EnterState(models.JobError)
	}
}

func (sm *JobSM) AddTransition(from string, to string, h StateHandler) {
	_, ok := sm.Transitions[from]
	if !ok {
		sm.Transitions[from] = make(map[string]struct{})
	}
	sm.Transitions[from][to] = struct{}{}
	sm.Handlers[to] = h
}

func (sm *JobSM) RemoveTransition(from string, to string) {
	_, ok := sm.Transitions[from]
	if !ok {
		return
	}
	delete(sm.Transitions[from], to)
}

func (sm *JobSM) Stop(id int64) {
	log.Debugf("Trying to stop the job: %d", id)
	sm.lock.Lock()
	defer sm.lock.Unlock()
	//need to check if the sm switched to other job
	if id == sm.JobID {
		sm.desiredState = models.JobStopped
		log.Debugf("Desired state of job %d is set to stopped", id)
	} else {
		log.Debugf("State machine has switched to job %d, so the action to stop job %d will be ignored", sm.JobID, id)
	}
}

func (sm *JobSM) getDesiredState() string {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	return sm.desiredState
}

func (sm *JobSM) setDesiredState(s string) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	sm.desiredState = s
}

func (sm *JobSM) Init() {
	sm.lock = &sync.Mutex{}
	sm.Handlers = make(map[string]StateHandler)
	sm.Transitions = make(map[string]map[string]struct{})
	sm.ForcedStates = map[string]struct{}{
		models.JobError:    struct{}{},
		models.JobStopped:  struct{}{},
		models.JobCanceled: struct{}{},
	}
}

func (sm *JobSM) Reset(jid int64) error {
	//To ensure the new jobID is visible to the thread to stop the SM
	sm.lock.Lock()
	sm.JobID = jid
	sm.desiredState = ""
	sm.lock.Unlock()

	//init parms
	job, err := dao.GetRepJob(sm.JobID)
	if err != nil {
		return fmt.Errorf("Failed to get job, error: %v", err)
	}
	if job == nil {
		return fmt.Errorf("The job doesn't exist in DB, job id: %d", sm.JobID)
	}
	policy, err := dao.GetRepPolicy(job.PolicyID)
	if err != nil {
		return fmt.Errorf("Failed to get policy, error: %v", err)
	}
	if policy == nil {
		return fmt.Errorf("The policy doesn't exist in DB, policy id:%d", job.PolicyID)
	}
	sm.Parms = &RepJobParm{
		LocalRegURL: config.LocalHarborURL(),
		Repository:  job.Repository,
		Tags:        job.TagList,
		Enabled:     policy.Enabled,
		Operation:   job.Operation,
	}
	if policy.Enabled == 0 {
		//worker will cancel this job
		return nil
	}
	target, err := dao.GetRepTarget(policy.TargetID)
	if err != nil {
		return fmt.Errorf("Failed to get target, error: %v", err)
	}
	if target == nil {
		return fmt.Errorf("The target doesn't exist in DB, target id: %d", policy.TargetID)
	}
	sm.Parms.TargetURL = target.URL
	sm.Parms.TargetUsername = target.Username
	pwd := target.Password

	if len(pwd) != 0 {
		pwd, err = uti.ReversibleDecrypt(pwd)
		if err != nil {
			return fmt.Errorf("failed to decrypt password: %v", err)
		}
	}

	sm.Parms.TargetPassword = pwd

	//init states handlers
	sm.Logger = utils.NewLogger(sm.JobID)
	sm.Handlers = make(map[string]StateHandler)
	sm.Transitions = make(map[string]map[string]struct{})
	sm.CurrentState = models.JobPending

	sm.AddTransition(models.JobPending, models.JobRunning, StatusUpdater{DummyHandler{JobID: sm.JobID}, models.JobRunning})
	sm.Handlers[models.JobError] = StatusUpdater{DummyHandler{JobID: sm.JobID}, models.JobError}
	sm.Handlers[models.JobStopped] = StatusUpdater{DummyHandler{JobID: sm.JobID}, models.JobStopped}

	switch sm.Parms.Operation {
	case models.RepOpTransfer:
		err = addImgTransferTransition(sm)
	case models.RepOpDelete:
		err = addImgDeleteTransition(sm)
	default:
		err = fmt.Errorf("unsupported operation: %s", sm.Parms.Operation)
	}

	return err
}

func addImgTransferTransition(sm *JobSM) error {
	base, err := replication.InitBaseHandler(sm.Parms.Repository, sm.Parms.LocalRegURL, config.UISecret(),
		sm.Parms.TargetURL, sm.Parms.TargetUsername, sm.Parms.TargetPassword,
		sm.Parms.Tags, sm.Logger)
	if err != nil {
		return err
	}
	sm.AddTransition(models.JobRunning, replication.StateCheck, &replication.Checker{BaseHandler: base})
	sm.AddTransition(replication.StateCheck, replication.StatePullManifest, &replication.ManifestPuller{BaseHandler: base})
	sm.AddTransition(replication.StatePullManifest, replication.StateTransferBlob, &replication.BlobTransfer{BaseHandler: base})
	sm.AddTransition(replication.StatePullManifest, models.JobFinished, &StatusUpdater{DummyHandler{JobID: sm.JobID}, models.JobFinished})
	sm.AddTransition(replication.StateTransferBlob, replication.StatePushManifest, &replication.ManifestPusher{BaseHandler: base})
	sm.AddTransition(replication.StatePushManifest, replication.StatePullManifest, &replication.ManifestPuller{BaseHandler: base})
	return nil
}

func addImgDeleteTransition(sm *JobSM) error {
	deleter := replication.NewDeleter(sm.Parms.Repository, sm.Parms.Tags, sm.Parms.TargetURL,
		sm.Parms.TargetUsername, sm.Parms.TargetPassword, sm.Logger)

	sm.AddTransition(models.JobRunning, replication.StateDelete, deleter)
	sm.AddTransition(replication.StateDelete, models.JobFinished, &StatusUpdater{DummyHandler{JobID: sm.JobID}, models.JobFinished})

	return nil
}